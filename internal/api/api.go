// Package api stellt die HTTP-API (poc/API.md) und die Web-UI bereit.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agw/internal/bgtask"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/oidc"
	"agw/internal/pool"
	"agw/internal/store"
	"agw/internal/worker"
)

type Server struct {
	M       *chat.Manager
	Pool    *pool.Pool[chat.Agent]
	Cat     *config.Catalog
	Env     config.Env
	Web     fs.FS // Inhalt von web/dist; nil ohne gebaute UI
	Blocked []*net.IPNet

	// Token schützt die API. Die Sandbox kennt es nicht; sie könnte die API
	// sonst über host.docker.internal erreichen und etwa eigene Uploads bestätigen.
	Token        string
	AllowedHosts []string // erlaubte Host-Kopfzeilen (Schutz gegen DNS-Rebinding)

	// Images liefert Anzeige-Bilder; nil heißt M (für Tests austauschbar).
	Images ImageOpener

	// OIDC: Anmeldung über den Keycloak der Plattform (AGW_AUTH_MODE=oidc); nil: token-Modus.
	// Dann gehören Chats dem angemeldeten Nutzer, und Token gilt nicht.
	OIDC *oidc.Service
	// FrameAncestors: Herkünfte, die die UI einbetten dürfen (leer: frame-ancestors 'none').
	FrameAncestors []string
}

type userKey struct{}

// UserFrom liefert den angemeldeten Nutzer (nur im oidc-Modus).
func UserFrom(ctx context.Context) (oidc.User, bool) {
	u, ok := ctx.Value(userKey{}).(oidc.User)
	return u, ok
}

// ImageOpener liefert ein Anzeige-Bild einer Antwort (siehe chat.Manager.OpenImage).
type ImageOpener interface {
	OpenImage(ctx context.Context, chatID, msg, path string) (store.ChatImage, io.ReadCloser, error)
}

// base ist der Pfad der UI für den Browser ("" oder etwa "/agent", aus AGW_PUBLIC_URL). Ein Proxy
// schneidet ihn vor dem Orchestrator ab; Weiterleitungen, Cookies und Anmeldelinks brauchen ihn.
func (s *Server) base() string {
	if s.OIDC != nil {
		return s.OIDC.Base()
	}
	return s.Env.BasePath
}

// CookieName ist das Anmelde-Cookie der Web-UI.
const CookieName = "agw_token"

const cspBase = "default-src 'self'; img-src 'self' data:; connect-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; font-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors "

// csp: frame-ancestors aus AGW_FRAME_ANCESTORS (Einbettung in die Plattform), sonst 'none'.
func (s *Server) csp() string {
	if len(s.FrameAncestors) == 0 {
		return cspBase + "'none'"
	}
	return cspBase + strings.Join(s.FrameAncestors, " ")
}

// auth setzt Sicherheitskopfzeilen und verlangt für /api/ das Token (Bearer
// oder Cookie), einen erlaubten Host und bei ändernden Anfragen gleiche Herkunft.
func (s *Server) auth(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	policy := s.csp()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if len(s.AllowedHosts) > 0 && !slices.Contains(s.AllowedHosts, r.Host) {
			slog.Warn("API: fremder Host abgewiesen", "host", r.Host)
			writeErr(w, http.StatusForbidden, "unbekannter Host")
			return
		}
		if s.OIDC != nil {
			u, ok := s.OIDC.SessionUser(r)
			if !ok {
				// login sagt der UI, wohin sie zur stillen Anmeldung (prompt=none) navigiert.
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "nicht angemeldet", "login": s.base() + oidc.LoginPath})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		} else if !s.tokenOK(r) {
			writeErr(w, http.StatusUnauthorized, "nicht angemeldet: den Anmeldelink aus ./dev.sh start öffnen")
			return
		}
		if err := cop.Check(r); err != nil {
			slog.Warn("API: Cross-Origin-Anfrage abgewiesen", "fehler", err)
			writeErr(w, http.StatusForbidden, "Cross-Origin-Anfrage abgewiesen")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tokenOK(r *http.Request) bool {
	if s.Token == "" {
		return false
	}
	given := ""
	if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		given = b
	} else if c, err := r.Cookie(CookieName); err == nil {
		given = c.Value
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.Token)) == 1
}

// login setzt das Anmelde-Cookie und leitet auf die UI weiter.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.OIDC != nil {
		http.Redirect(w, r, s.base()+oidc.LoginPath, http.StatusSeeOther) // /login?token= gilt nur im token-Modus
		return
	}
	given := r.URL.Query().Get("token")
	if s.Token == "" || subtle.ConstantTimeCompare([]byte(given), []byte(s.Token)) != 1 {
		http.Error(w, "Anmeldung fehlgeschlagen: Token falsch", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: s.Token, Path: s.base() + "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
	http.Redirect(w, r, s.base()+"/", http.StatusSeeOther)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Routen eines Chats und einer Bestätigung prüfen zentral, ob sie dem Nutzer gehören (oidc).
	chat := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.ownChat(h)) }
	mux.HandleFunc("GET /login", s.login)
	if s.OIDC != nil {
		mux.Handle("/oidc/", s.OIDC.Handler())
	}
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/models", s.models)
	mux.HandleFunc("GET /api/variants", s.variants)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/pool", s.pool)
	mux.HandleFunc("GET /api/chats", s.listChats)
	mux.HandleFunc("POST /api/chats", s.createChat)
	chat("GET /api/chats/{id}", s.getChat)
	chat("POST /api/chats/{id}/messages", s.send)
	chat("GET /api/chats/{id}/queue", s.queue)
	chat("POST /api/chats/{id}/queue/send", s.flushQueue)
	chat("DELETE /api/chats/{id}/queue/{qid}", s.unqueue)
	chat("POST /api/chats/{id}/abort", s.action(s.M.Abort))
	chat("POST /api/chats/{id}/suspend", s.action(s.M.Suspend))
	chat("POST /api/chats/{id}/internet", s.internet)
	chat("POST /api/chats/{id}/model", s.setModel)
	chat("GET /api/chats/{id}/tools/running", s.runningTools)
	chat("GET /api/chats/{id}/web_requests", s.webRequests)
	chat("POST /api/chats/{id}/tools/{call}/stop", s.stopTool)
	chat("POST /api/chats/{id}/tools/{call}/background", s.backgroundTool)
	chat("POST /api/chats/{id}/effort", s.setEffort)
	chat("POST /api/chats/{id}/autocompact", s.autocompact)
	chat("POST /api/chats/{id}/subagents", s.maxSubagents)
	chat("GET /api/chats/{id}/llm_calls", s.llmCalls)
	chat("GET /api/chats/{id}/tool_executions", s.toolExecutions)
	chat("GET /api/chats/{id}/background", s.background)
	chat("POST /api/chats/{id}/background/{bg}/stop", s.stopBackground)
	chat("GET /api/chats/{id}/commands", s.commands)
	chat("POST /api/chats/{id}/commands", s.runCommand)
	chat("GET /api/chats/{id}/session", s.session)
	chat("GET /api/chats/{id}/artifacts", s.artifacts)
	chat("GET /api/chats/{id}/artifacts/{name}", s.download)
	chat("POST /api/chats/{id}/files", s.upload)
	chat("GET /api/chats/{id}/images", s.image)
	chat("GET /api/chats/{id}/events", s.events)
	mux.HandleFunc("GET /api/approvals", s.approvals)
	mux.Handle("POST /api/approvals/{id}", s.ownApproval(s.decide))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "unbekannter Endpunkt")
	})
	if s.Web != nil {
		mux.Handle("/", spa(s.Web))
	}
	return s.guard(s.auth(mux))
}

// Owners beantwortet, wem Chats und Bestätigungen gehören (chat.Manager).
type Owners interface {
	ChatOwner(ctx context.Context, chatID string) (string, error)
	ApprovalChat(ctx context.Context, approvalID string) (string, error)
}

func (s *Server) owners() Owners { return s.M }

// owns sagt, ob der Chat dem angemeldeten Nutzer gehört. Im token-Modus gehört jeder Chat dem
// Inhaber des Tokens. Fremde und unbekannte Chats sind gleich (nicht gefunden), damit sich Kennungen
// fremder Chats nicht erraten lassen.
func (s *Server) owns(ctx context.Context, chatID string) (bool, error) {
	if s.OIDC == nil {
		return true, nil
	}
	u, ok := UserFrom(ctx)
	if !ok || u.Sub == "" {
		return false, nil
	}
	owner, err := s.owners().ChatOwner(ctx, chatID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return owner == u.Sub, nil
}

// ownChat lässt eine Route unter /api/chats/{id} nur für Chats des Nutzers durch (sonst 404).
func (s *Server) ownChat(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.owns(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
			return
		}
		next(w, r)
	})
}

// ownApproval lässt eine Bestätigung nur entscheiden, wenn ihr Chat dem Nutzer gehört (sonst 404).
func (s *Server) ownApproval(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.OIDC != nil {
			chatID, err := s.owners().ApprovalChat(r.Context(), r.PathValue("id"))
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				fail(w, err)
				return
			}
			ok := false
			if err == nil {
				if ok, err = s.owns(r.Context(), chatID); err != nil {
					fail(w, err)
					return
				}
			}
			if !ok {
				writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
				return
			}
		}
		next(w, r)
	})
}

// me: angemeldeter Nutzer und Anmeldeart.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeJSON(w, 200, map[string]any{"mode": config.AuthToken})
		return
	}
	u, _ := UserFrom(r.Context())
	writeJSON(w, 200, map[string]any{"mode": config.AuthOIDC, "sub": u.Sub, "username": u.Username, "name": u.Name})
}

// guard weist Anfragen aus den Sandbox-Netzen ab. Die Sandboxen erreichen
// den Orchestrator über das interne Netz; die API ist nur für den Nutzer.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip != nil {
			for _, n := range s.Blocked {
				if n.Contains(ip) {
					slog.Warn("API-Zugriff aus Sandbox-Netz abgewiesen", "von", host, "pfad", r.URL.Path)
					writeErr(w, http.StatusForbidden, "kein Zugriff aus dem Sandbox-Netz")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func spa(files fs.FS) http.Handler {
	fsrv := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(files, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fsrv.ServeHTTP(w, r)
				return
			}
		}
		b, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "Web-UI nicht gebaut", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func errCode(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, chat.ErrImageUnavailable), errors.Is(err, chat.ErrNoForeground):
		return http.StatusNotFound
	case errors.Is(err, bgtask.ErrLimit):
		return http.StatusConflict
	case errors.Is(err, chat.ErrNoSlot):
		return http.StatusServiceUnavailable
	case errors.Is(err, chat.ErrPendingApproval), errors.Is(err, chat.ErrRunning),
		errors.Is(err, chat.ErrQueueDelivered), errors.Is(err, chat.ErrNotRunning):
		return http.StatusConflict
	case errors.Is(err, chat.ErrTooManyPending):
		return http.StatusTooManyRequests
	case errors.Is(err, chat.ErrUnknownModel), errors.Is(err, chat.ErrUnknownVariant), errors.Is(err, chat.ErrInvalid):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func fail(w http.ResponseWriter, err error) {
	// Modellwechsel gesperrt: die Zahlen mitgeben, damit die UI fragen kann, ob erst kompaktiert wird.
	var tooLarge *chat.ContextTooLargeError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "context_too_large", "details": tooLarge})
		return
	}
	code := errCode(err)
	if code >= 500 {
		slog.Error("API-Fehler", "fehler", err)
	}
	writeErr(w, code, err.Error())
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Cat.Models())
}

func (s *Server) variants(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, worker.Variants)
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	o := s.M.Options()
	writeJSON(w, 200, map[string]any{
		"internet_default":           o.InternetDefault,
		"approval_timeout_s":         int(o.ApprovalTimeout.Seconds()),
		"artifact_max_mb":            o.ArtifactMaxBytes >> 20,
		"idle_timeout_s":             int(o.IdleTimeout.Seconds()),
		"auto_compact_default":       o.AutoCompactDefault,
		"compact_reserve_tokens":     o.CompactReserveTokens,
		"compact_keep_recent_tokens": o.CompactKeepRecent,
		"max_subagents_default":      o.MaxSubagentsDefault,
		"max_subagents_limit":        o.MaxSubagentsLimit,
		"workspace_max_mb":           workspaceMaxMB(o.WorkspaceMaxBytes),
		"bg_wakes_per_hour":          o.BgWakesPerHour,
		"bg_keepalive_s":             int(o.BgKeepAlive.Seconds()),
		"auto_turns_max":             o.AutoTurnsMax,
		// Werkzeuge, deren Ausführung am Socket belegt wird (E9, L6): Die UI gleicht damit ab,
		// statt die Liste selbst zu führen.
		"executed_tools": chat.ExecutedToolNames(),
	})
}

// workspaceMaxMB: Grenze der Sicherung von /workspace in MB, 0 = keine Sicherung.
func workspaceMaxMB(b int64) int64 {
	switch {
	case b < 0:
		return 0
	case b == 0:
		return chat.DefaultWorkspaceMaxBytes >> 20
	}
	return b >> 20
}

type slotView struct {
	pool.Info
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	// Ausführungs-Sandbox des Platzes (E9)
	ExecContainerID   string `json:"exec_container_id,omitempty"`
	ExecContainerName string `json:"exec_container_name,omitempty"`
	ExecImage         string `json:"exec_image,omitempty"`
	ChatTitle         string `json:"chat_title,omitempty"`
	Internet          *bool  `json:"internet,omitempty"`
}

func (s *Server) pool(w http.ResponseWriter, r *http.Request) {
	infos := s.Pool.Snapshot()
	out := make([]slotView, 0, len(infos))
	active := 0
	for _, in := range infos {
		v := slotView{Info: in, Image: s.Env.Image}
		if sl, ok := s.Pool.Get(in.ID); ok && sl.Worker != nil {
			id := sl.Worker.ContainerID()
			if len(id) > 12 {
				id = id[:12]
			}
			v.ContainerID, v.ContainerName, v.Image = id, sl.Worker.ContainerName(), sl.Worker.Image()
			if x, ok := sl.Worker.(interface {
				ExecContainerID() string
				ExecContainerName() string
			}); ok {
				xid := x.ExecContainerID()
				if len(xid) > 12 {
					xid = xid[:12]
				}
				v.ExecContainerID, v.ExecContainerName, v.ExecImage = xid, x.ExecContainerName(), s.Env.Image
			}
		}
		if in.ChatID != "" {
			// Kennung und Titel fremder Chats sieht der Nutzer nicht (oidc).
			if ok, _ := s.owns(r.Context(), in.ChatID); !ok {
				v.ChatID, v.Activity = "", nil
				if in.State == pool.StateAssigned {
					active++
				}
				out = append(out, v)
				continue
			}
		}
		if in.ChatID != "" && in.State == pool.StateAssigned {
			if c, err := s.M.View(r.Context(), in.ChatID); err == nil {
				v.ChatTitle = c.Title
				on := c.Internet
				v.Internet = &on
				active++
			}
		}
		out = append(out, v)
	}
	tok, cost, _ := s.M.Totals(r.Context())
	writeJSON(w, 200, map[string]any{
		"slots":   out,
		"targets": s.Pool.Targets(),
		"totals":  map[string]any{"cost": cost, "tokens": tok, "chats_active": active},
	})
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	cs, err := s.M.List(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if u, ok := UserFrom(r.Context()); ok && s.OIDC != nil {
		own := make([]chat.ChatView, 0, len(cs))
		for _, c := range cs {
			if c.Owner == u.Sub {
				own = append(own, c)
			}
		}
		cs = own
	}
	writeJSON(w, 200, cs)
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var req chat.NewChat
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, 400, "ungültiges JSON")
		return
	}
	req.Owner = ""
	if s.OIDC != nil {
		u, ok := UserFrom(r.Context())
		if !ok || u.Sub == "" {
			writeErr(w, http.StatusUnauthorized, "nicht angemeldet")
			return
		}
		req.Owner = u.Sub
	}
	c, err := s.M.Create(r.Context(), req)
	if err != nil {
		if errors.Is(err, chat.ErrNoSlot) {
			writeErr(w, http.StatusServiceUnavailable, "Kein freier Platz im Pool, bitte kurz warten.")
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	c, err := s.M.View(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	msgs, err1 := s.M.Messages(ctx, id)
	arts, err2 := s.M.ListArtifacts(ctx, id)
	aps, err3 := s.M.Approvals(ctx, "", id)
	calls, err4 := s.M.SocketCalls(ctx, id)
	subs, err5 := s.M.SubagentEntries(ctx, id)
	subRuns, _ := s.M.SubagentRuns(ctx, id)
	queue, err6 := s.M.Queue(ctx, id)
	bg, err7 := s.M.BackgroundTasks(ctx, id)
	if err := errors.Join(err1, err2, err3, err4, err5, err6, err7); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"chat": c, "messages": msgs, "artifacts": arts, "approvals": aps, "socket_calls": calls, "subagent_entries": subs, "subagent_runs": subRuns, "queue": queue, "background": bg})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text        string   `json:"text"`
		Attachments []string `json:"attachments"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "ungültiges JSON")
		return
	}
	if len(req.Attachments) > 20 {
		writeErr(w, 400, "höchstens 20 Anhänge je Nachricht")
		return
	}
	res, err := s.M.SendWithAttachments(r.Context(), r.PathValue("id"), req.Text, req.Attachments)
	if err != nil {
		sendFail(w, err)
		return
	}
	writeSend(w, res)
}

func sendFail(w http.ResponseWriter, err error) {
	if errors.Is(err, chat.ErrNoSlot) {
		writeErr(w, http.StatusServiceUnavailable, "Kein freier Platz im Pool, bitte kurz warten.")
		return
	}
	fail(w, err)
}

func writeSend(w http.ResponseWriter, res chat.SendResult) {
	out := map[string]any{"ok": true, "resumed": res.Resumed, "queued": res.Queued}
	if res.Queued {
		out["queue_id"] = res.QueueID
	}
	writeJSON(w, 200, out)
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.View(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	q, err := s.M.Queue(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, q)
}

func (s *Server) flushQueue(w http.ResponseWriter, r *http.Request) {
	res, err := s.M.FlushQueue(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, chat.ErrRunning) {
			writeErr(w, http.StatusConflict, "Der Agent arbeitet gerade; die Nachrichten gehen mit dem Ende des Laufs.")
			return
		}
		sendFail(w, err)
		return
	}
	writeSend(w, res)
}

func (s *Server) unqueue(w http.ResponseWriter, r *http.Request) {
	if err := s.M.Unqueue(r.Context(), r.PathValue("id"), r.PathValue("qid")); err != nil {
		if errors.Is(err, chat.ErrQueueDelivered) {
			writeErr(w, http.StatusConflict, "Die Nachricht ist schon an den Agenten übergeben.")
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) action(f func(context.Context, string) (chat.ChatView, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := f(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, c)
	}
}

func (s *Server) internet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Enabled == nil {
		writeErr(w, 400, `erwartet {"enabled": true|false}`)
		return
	}
	c, err := s.M.SetInternet(r.Context(), r.PathValue("id"), *req.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) webRequests(w http.ResponseWriter, r *http.Request) {
	list, err := s.M.WebRequests(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) runningTools(w http.ResponseWriter, r *http.Request) {
	if _, err := s.M.View(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"tool_call_ids": s.M.RunningTools(r.PathValue("id"))})
}

func (s *Server) stopTool(w http.ResponseWriter, r *http.Request) {
	if err := s.M.StopTool(r.Context(), r.PathValue("id"), r.PathValue("call")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) backgroundTool(w http.ResponseWriter, r *http.Request) {
	t, err := s.M.BackgroundTool(r.Context(), r.PathValue("id"), r.PathValue("call"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) setModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model        string `json:"model"`
		CompactFirst bool   `json:"compact_first"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Model == "" {
		writeErr(w, 400, `erwartet {"model": "anbieter/modell", "compact_first": false}`)
		return
	}
	c, err := s.M.SetModel(r.Context(), r.PathValue("id"), req.Model, req.CompactFirst)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) setEffort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Level == "" {
		writeErr(w, 400, `erwartet {"level": "off|minimal|low|medium|high|xhigh|max"}`)
		return
	}
	c, err := s.M.SetThinkingLevel(r.Context(), r.PathValue("id"), req.Level)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) autocompact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Enabled == nil {
		writeErr(w, 400, `erwartet {"enabled": true|false}`)
		return
	}
	c, err := s.M.SetAutoCompact(r.Context(), r.PathValue("id"), *req.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) maxSubagents(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Max *int `json:"max"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Max == nil {
		writeErr(w, 400, `erwartet {"max": <Zahl>}`)
		return
	}
	c, err := s.M.SetMaxSubagents(r.Context(), r.PathValue("id"), *req.Max)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) llmCalls(w http.ResponseWriter, r *http.Request) {
	calls, err := s.M.LLMCalls(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, calls)
}

// toolExecutions: Abgleich angefordert (Proxy) ↔ ausgeführt (Orchestrator) je toolCallId (E9).
// background: Hintergrundaufgaben des Chats (laufende mit dem aktuellen Stand des Platzes).
func (s *Server) background(w http.ResponseWriter, r *http.Request) {
	list, err := s.M.BackgroundTasks(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

// stopBackground: Hintergrundaufgabe auf Wunsch des Nutzers beenden; 409, wenn sie nicht läuft.
func (s *Server) stopBackground(w http.ResponseWriter, r *http.Request) {
	t, err := s.M.StopBackground(r.Context(), r.PathValue("id"), r.PathValue("bg"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) toolExecutions(w http.ResponseWriter, r *http.Request) {
	rec, err := s.M.ToolExecutions(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rec)
}

func (s *Server) commands(w http.ResponseWriter, r *http.Request) {
	c, err := s.M.Commands(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		writeErr(w, 400, `erwartet {"command": "/…"}`)
		return
	}
	res, err := s.M.RunCommand(r.Context(), r.PathValue("id"), req.Command)
	if err != nil {
		if errors.Is(err, chat.ErrNoSlot) {
			writeErr(w, http.StatusServiceUnavailable, "Kein freier Platz im Pool, bitte kurz warten.")
			return
		}
		if errors.Is(err, chat.ErrRunning) {
			writeErr(w, http.StatusConflict, "Der Agent arbeitet gerade; /compact geht erst danach.")
			return
		}
		fail(w, err)
		return
	}
	writeSend(w, res)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	b, err := s.M.Session(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.jsonl"`, r.PathValue("id")))
	_, _ = w.Write(b)
}

func (s *Server) artifacts(w http.ResponseWriter, r *http.Request) {
	a, err := s.M.ListArtifacts(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != store.KindInput {
		kind = store.KindOutput
	}
	name := r.PathValue("name")
	rc, size, err := s.M.OpenArtifact(r.Context(), r.PathValue("id"), kind, name)
	if err != nil {
		fail(w, err)
		return
	}
	defer rc.Close()
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	_, _ = io.Copy(w, rc)
}

// image liefert ein Bild, das der Agent in einer Antwort zeigt. Der Typ kommt
// aus den Magic Bytes (nur PNG, JPEG, GIF, WebP), nie aus der Dateiendung.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, msg := q.Get("path"), q.Get("msg")
	if p == "" || msg == "" {
		writeErr(w, 400, "erwartet ?path=<Pfad in der Sandbox>&msg=<Kennung der Antwort>")
		return
	}
	var src ImageOpener = s.M
	if s.Images != nil {
		src = s.Images
	}
	im, rc, err := src.OpenImage(r.Context(), r.PathValue("id"), msg, p)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store") // später vielleicht verfügbar
		fail(w, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", im.ContentType)
	h.Set("Content-Length", fmt.Sprint(im.Size))
	h.Set("X-Content-Type-Options", "nosniff")
	// Je (Antwort, Pfad) gilt die erste Sicherung; der Inhalt ändert sich nicht mehr.
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(im.Path)}))
	_, _ = io.Copy(w, rc)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	max := s.M.Options().ArtifactMaxBytes
	r.Body = http.MaxBytesReader(w, r.Body, 20*max+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "Upload nicht lesbar: "+err.Error())
		return
	}
	var out []store.Artifact
	for _, fh := range r.MultipartForm.File["file"] {
		if fh.Size > max {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s ist größer als %d MB", fh.Filename, max>>20))
			return
		}
		f, err := fh.Open()
		if err != nil {
			fail(w, err)
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			fail(w, err)
			return
		}
		a, err := s.M.AddInput(r.Context(), r.PathValue("id"), fh.Filename, data)
		if err != nil {
			fail(w, err)
			return
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		writeErr(w, 400, "keine Datei im Feld file")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.View(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "Streaming nicht möglich")
		return
	}
	ch, cancel := s.M.Subscribe(id)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, ": verbunden\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chatID := r.URL.Query().Get("chat")
	if chatID != "" {
		if ok, err := s.owns(ctx, chatID); err != nil || !ok {
			if err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, 200, []store.Approval{})
			return
		}
	}
	a, err := s.M.Approvals(ctx, r.URL.Query().Get("state"), chatID)
	if err != nil {
		fail(w, err)
		return
	}
	if s.OIDC != nil && chatID == "" {
		mine := map[string]bool{}
		own := make([]store.Approval, 0, len(a))
		for _, ap := range a {
			ok, seen := mine[ap.ChatID]
			if !seen {
				if ok, err = s.owns(ctx, ap.ChatID); err != nil {
					fail(w, err)
					return
				}
				mine[ap.ChatID] = ok
			}
			if ok {
				own = append(own, ap)
			}
		}
		a = own
	}
	writeJSON(w, 200, a)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approve *bool `json:"approve"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Approve == nil {
		writeErr(w, 400, `erwartet {"approve": true|false}`)
		return
	}
	a, err := s.M.Decide(r.Context(), r.PathValue("id"), *req.Approve)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

// ParseSubnets liest eine kommagetrennte Liste von CIDR-Angaben.
func ParseSubnets(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

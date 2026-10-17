// Package api provides the HTTP API (poc/API.md) and the web UI.
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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agw/internal/artifacts"
	"agw/internal/bgtask"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/oidc"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/store"
	"agw/internal/worker"
)

type Server struct {
	M       *chat.Manager
	Pool    *pool.Pool[chat.Agent]
	Cat     *config.Catalog
	Env     config.Env
	Web     fs.FS // contents of web/dist; nil without a built UI
	Blocked []*net.IPNet

	// Token protects the API. The sandbox does not know it; otherwise it could reach the API
	// via host.docker.internal and, for example, approve its own uploads.
	Token        string
	AllowedHosts []string // allowed Host headers (protection against DNS rebinding)

	// Images serves display images; nil means M (replaceable for tests).
	Images ImageOpener
	// Artifacts serves the files of a chat; nil means M (replaceable for tests).
	Artifacts ArtifactOpener

	// OIDC: login through the platform's Keycloak (AGW_AUTH_MODE=oidc); nil: token mode.
	// Chats then belong to the logged-in user, and Token does not apply.
	OIDC *oidc.Service
	// FrameAncestors: origins allowed to embed the UI (empty: frame-ancestors 'none').
	FrameAncestors []string

	// Platform is the binding to the Agri-Gaia platform for GET /api/platform (nil: binding off).
	Platform *platform.Client
	// ExpFixtures seeds and removes the fixture objects of an experiment cell (X4, issue #129);
	// nil: the experiment accounts are not configured and the routes answer 503.
	ExpFixtures ExpFixtures
	// ExpAPIToken protects /api/exp/*. It is separate from Token: the experiment runner on the server
	// uses it, the browser and the sandbox never see it (X1.3, X4).
	ExpAPIToken string
	// ExpAccount resolves the experiment account for ExpAPIToken (sub for the owner, user name for
	// the permission of experiment chats, X1.3). Nil: the runner's chat routes are off; the fixture
	// routes still work with the key alone.
	ExpAccount func(context.Context) (oidc.User, error)
	// OwnerSource answers who owns chats and approvals; nil means M (replaceable for tests).
	OwnerSource Owners
}

type userKey struct{}

// expKey marks a request that came in with the experiment key instead of a user session (X1.3).
type expKey struct{}

// ExpKeyFrom reports whether the request was authenticated with the experiment key.
func ExpKeyFrom(ctx context.Context) bool {
	_, ok := ctx.Value(expKey{}).(bool)
	return ok
}

// withExpKey marks a context as coming from the experiment key (the middleware does this; tests too).
func withExpKey(ctx context.Context) context.Context {
	return context.WithValue(ctx, expKey{}, true)
}

// UserFrom returns the logged-in user (oidc mode only).
func UserFrom(ctx context.Context) (oidc.User, bool) {
	u, ok := ctx.Value(userKey{}).(oidc.User)
	return u, ok
}

// ImageOpener serves a display image of a response (see chat.Manager.OpenImage).
type ImageOpener interface {
	OpenImage(ctx context.Context, chatID, msg, path string) (store.ChatImage, io.ReadCloser, error)
}

// ArtifactOpener serves a stored file of a chat (see chat.Manager.OpenArtifact).
type ArtifactOpener interface {
	OpenArtifact(ctx context.Context, chatID, kind, name string) (io.ReadCloser, int64, error)
}

// base is the UI's path for the browser ("" or e.g. "/agent", from AGW_PUBLIC_URL). A proxy
// strips it before the orchestrator; redirects, cookies and login links need it.
func (s *Server) base() string {
	if s.OIDC != nil {
		return s.OIDC.Base()
	}
	return s.Env.BasePath
}

// CookieName is the web UI's login cookie.
const CookieName = "agw_token"

const cspBase = "default-src 'self'; img-src 'self' data:; connect-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; font-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors "

// csp: frame-ancestors from AGW_FRAME_ANCESTORS (embedding in the platform), otherwise 'none'.
func (s *Server) csp() string {
	if len(s.FrameAncestors) == 0 {
		return cspBase + "'none'"
	}
	return cspBase + strings.Join(s.FrameAncestors, " ")
}

// auth sets security headers and, for /api/, requires the token (bearer
// or cookie), an allowed host and, for modifying requests, the same origin.
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
			slog.Warn("API: foreign host refused", "host", r.Host)
			// Name the host and the cause, never the allowed list: a caller who uses 127.0.0.1
			// from inside a container should learn that the URL, not the key, is wrong (#155).
			hint := "check AGW_ALLOWED_HOSTS and the URL"
			if s.Env.PublicURL != "" {
				hint = "use the instance's URL " + s.Env.PublicURL
			}
			writeErr(w, http.StatusForbidden, `unknown host "`+r.Host+`" (not in AGW_ALLOWED_HOSTS; `+hint+`)`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/exp/") {
			// The experiment runner carries its own key (AGW_EXP_API_TOKEN, X1.3/X4): it reaches the
			// fixture routes and nothing else, and a session of the UI does not reach them.
			if !s.expTokenOK(r) {
				writeErr(w, http.StatusUnauthorized, "experiment API: missing or wrong AGW_EXP_API_TOKEN")
				return
			}
			if s.OIDC != nil && s.ExpAccount != nil {
				// X1: the runner's routes also act as the experiment account (owner checks).
				u, err := s.ExpAccount(r.Context())
				if err != nil {
					slog.Warn("API: experiment account not reachable", "error", err)
					writeErr(w, http.StatusServiceUnavailable, "experiment account not reachable")
					return
				}
				ctx := withExpKey(r.Context())
				r = r.WithContext(context.WithValue(ctx, userKey{}, u))
			}
		} else if s.expTokenOK(r) && s.OIDC != nil && s.ExpAccount != nil {
			// X1.3: the same key creates experiment chats and reads their chats, on its surface only.
			u, err := s.ExpAccount(r.Context())
			if err != nil {
				slog.Warn("API: experiment account not reachable", "error", err)
				writeErr(w, http.StatusServiceUnavailable, "experiment account not reachable")
				return
			}
			if !expRouteAllowed(r) {
				writeErr(w, http.StatusForbidden, "the experiment key does not reach this route")
				return
			}
			ctx := withExpKey(r.Context())
			r = r.WithContext(context.WithValue(ctx, userKey{}, u))
		} else if s.OIDC != nil {
			u, ok := s.OIDC.SessionUser(r)
			if !ok {
				// login tells the UI where to navigate for the silent login (prompt=none).
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in", "login": s.base() + oidc.LoginPath})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		} else if !s.tokenOK(r) {
			writeErr(w, http.StatusUnauthorized, "not logged in: open the login link from ./dev.sh start")
			return
		}
		if err := cop.Check(r); err != nil {
			slog.Warn("API: cross-origin request refused", "error", err)
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
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

// expTokenOK checks the bearer key of the experiment routes. It is only accepted as a bearer
// header, never as a cookie: the browser has no business on these routes.
func (s *Server) expTokenOK(r *http.Request) bool {
	if s.ExpAPIToken == "" {
		return false
	}
	given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(given), []byte(s.ExpAPIToken)) == 1
}

// expRouteAllowed is the smallest surface of the key outside /api/exp/ (X1.3): create an experiment
// chat, read the chats the account owns and read the model catalogue (the runner and
// `agw exp validate` check the plan's model IDs against it, #156). The runner's own routes are
// under /api/exp/.
func expRouteAllowed(r *http.Request) bool {
	p := r.URL.Path
	return (r.Method == http.MethodPost && p == "/api/chats") ||
		(r.Method == http.MethodGet && (strings.HasPrefix(p, "/api/chats/") || p == "/api/models"))
}

// login sets the login cookie and redirects to the UI.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.OIDC != nil {
		http.Redirect(w, r, s.base()+oidc.LoginPath, http.StatusSeeOther) // /login?token= only applies in token mode
		return
	}
	given := r.URL.Query().Get("token")
	if s.Token == "" || subtle.ConstantTimeCompare([]byte(given), []byte(s.Token)) != 1 {
		http.Error(w, "login failed: wrong token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: s.Token, Path: s.base() + "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
	http.Redirect(w, r, s.base()+"/", http.StatusSeeOther)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Routes of a chat and of an approval check centrally whether they belong to the user (oidc).
	chat := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.ownChat(h)) }
	// writes also refuses a chat in the trash with 409 chat_in_trash (issue #76); reads stay open.
	writes := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.ownChat(s.writable(h))) }
	mux.HandleFunc("GET /login", s.login)
	if s.OIDC != nil {
		mux.Handle("/oidc/", s.OIDC.Handler())
	}
	mux.HandleFunc("GET /api/exp/export", s.expExport)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/models", s.models)
	mux.HandleFunc("GET /api/variants", s.variants)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/pool", s.pool)
	mux.HandleFunc("GET /api/platform", s.platformStatus)
	// X4 (issue #129): fixtures of an experiment cell, only through AGW_EXP_API_TOKEN.
	mux.HandleFunc("POST /api/exp/fixtures/seed", s.expSeed)
	mux.HandleFunc("POST /api/exp/fixtures/unseed", s.expUnseed)
	mux.HandleFunc("POST /api/exp/fixtures/verify", s.expVerify)
	mux.HandleFunc("GET /api/exp/fixtures", s.expList)
	mux.HandleFunc("GET /api/chats", s.listChats)
	mux.HandleFunc("POST /api/chats", s.createChat)
	chat("GET /api/chats/{id}", s.getChat)
	writes("POST /api/chats/{id}/messages", s.send)
	chat("GET /api/chats/{id}/queue", s.queue)
	writes("POST /api/chats/{id}/queue/send", s.flushQueue)
	writes("DELETE /api/chats/{id}/queue/{qid}", s.unqueue)
	writes("POST /api/chats/{id}/abort", s.action(s.M.Abort))
	writes("POST /api/chats/{id}/suspend", s.action(s.M.Suspend))
	writes("POST /api/chats/{id}/resume", s.action(s.M.Resume))
	writes("POST /api/chats/{id}/internet", s.internet)
	writes("POST /api/chats/{id}/model", s.setModel)
	chat("GET /api/chats/{id}/tools/running", s.runningTools)
	chat("GET /api/chats/{id}/web_requests", s.webRequests)
	writes("POST /api/chats/{id}/tools/{call}/stop", s.stopTool)
	writes("POST /api/chats/{id}/tools/{call}/background", s.backgroundTool)
	writes("POST /api/chats/{id}/effort", s.setEffort)
	writes("POST /api/chats/{id}/autocompact", s.autocompact)
	writes("POST /api/chats/{id}/subagents", s.subagentLimitGone)
	chat("GET /api/chats/{id}/subagent-runs", s.subagentRuns)
	writes("POST /api/chats/{id}/subagent-runs/{run}/messages", s.subagentMessage)
	// Stops one running subagent without stopping the chat (issue #120, L3).
	writes("POST /api/chats/{id}/subagent-runs/{run}/stop", s.subagentStop)
	chat("GET /api/chats/{id}/plans", s.plans)
	// R0 (issue #112): the result checks of a chat with their reports, for the check card.
	chat("GET /api/chats/{id}/assessments", s.assessments)
	// V4 (issue #112): the runs and jobs of pre-annotation with their results, for the cards in the
	// history. Own chats only; the route reads the database and never wakes the chat.
	chat("GET /api/chats/{id}/annotation", s.annotationResults)
	// A-E1: the user's controls of an autonomous plan; the agent has no tool for them.
	writes("POST /api/chats/{id}/plans/{plan}/pause", s.planControl(func(ctx context.Context, chatID, planID string) (store.Plan, error) {
		return s.M.PausePlan(ctx, chatID, planID, "user")
	}))
	writes("POST /api/chats/{id}/plans/{plan}/resume", s.planControl(func(ctx context.Context, chatID, planID string) (store.Plan, error) {
		return s.M.ResumePlan(ctx, chatID, planID)
	}))
	writes("POST /api/chats/{id}/plans/{plan}/cancel", s.planControl(func(ctx context.Context, chatID, planID string) (store.Plan, error) {
		return s.M.CancelPlan(ctx, chatID, planID)
	}))
	// A-E2: observed platform jobs (the gateway's watcher, not the agent). The list carries the state
	// of every job of a chat including the progress of a waiting plan step; "stop" is the user's own
	// action for a waiting step and for a single job outside a plan (D2.9).
	chat("GET /api/chats/{id}/jobs", s.jobs)
	writes("POST /api/chats/{id}/plans/{plan}/steps/{idx}/stop", s.stopPlanStep)
	writes("POST /api/chats/{id}/jobs/{job}/stop", s.stopJob)
	chat("GET /api/chats/{id}/llm_calls", s.llmCalls)
	chat("GET /api/chats/{id}/tool_executions", s.toolExecutions)
	chat("GET /api/chats/{id}/background", s.background)
	writes("POST /api/chats/{id}/background/{bg}/stop", s.stopBackground)
	chat("GET /api/chats/{id}/commands", s.commands)
	writes("POST /api/chats/{id}/commands", s.runCommand)
	chat("GET /api/chats/{id}/session", s.session)
	chat("GET /api/chats/{id}/artifacts", s.artifacts)
	chat("GET /api/chats/{id}/artifacts/{name}", s.download)
	writes("POST /api/chats/{id}/files", s.upload)
	// L4/#121: an input that was not sent with a message can be taken back (the cross on the tile, a cancelled upload).
	writes("DELETE /api/chats/{id}/inputs/{name}", s.removeInput)
	chat("GET /api/chats/{id}/images", s.image)
	chat("GET /api/chats/{id}/events", s.events)
	chat("POST /api/chats/{id}/trash", s.trashChat)
	chat("POST /api/chats/{id}/restore", s.restoreChat)
	chat("DELETE /api/chats/{id}", s.deleteChat)
	mux.HandleFunc("GET /api/trash", s.listTrash)
	mux.HandleFunc("POST /api/trash/empty", s.emptyTrash)
	mux.HandleFunc("GET /api/approvals", s.approvals)
	mux.HandleFunc("GET /api/events", s.allEvents)
	mux.HandleFunc("GET /api/activity", s.activity)
	mux.Handle("POST /api/approvals/{id}", s.ownApproval(s.approvalWritable(s.decide)))
	s.registerExp(mux) // X1: the experiment runner, only with the experiment key
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "unknown endpoint")
	})
	if s.Web != nil {
		mux.Handle("/", spa(s.Web))
	}
	return s.guard(s.auth(mux))
}

// Owners answers who owns chats and approvals (chat.Manager).
type Owners interface {
	ChatOwner(ctx context.Context, chatID string) (string, error)
	ApprovalChat(ctx context.Context, approvalID string) (string, error)
}

func (s *Server) owners() Owners {
	if s.OwnerSource != nil {
		return s.OwnerSource
	}
	return s.M
}

// owns reports whether the chat belongs to the logged-in user. In token mode every chat belongs to
// the holder of the token. Foreign and unknown chats look the same (not found), so that identifiers
// of foreign chats cannot be guessed.
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

// ownChat lets a route under /api/chats/{id} through only for the user's chats (otherwise 404).
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

// ownApproval only lets an approval be decided if its chat belongs to the user (otherwise 404).
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

// me: logged-in user and login mode.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeJSON(w, 200, map[string]any{"mode": config.AuthToken})
		return
	}
	u, _ := UserFrom(r.Context())
	writeJSON(w, 200, map[string]any{"mode": config.AuthOIDC, "sub": u.Sub, "username": u.Username, "name": u.Name})
}

// guard refuses requests from the sandbox networks. The sandboxes reach
// the orchestrator via the internal network; the API is for the user only.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip != nil {
			for _, n := range s.Blocked {
				if n.Contains(ip) {
					slog.Warn("API access from sandbox network refused", "from", host, "path", r.URL.Path)
					writeErr(w, http.StatusForbidden, "no access from the sandbox network")
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
			http.Error(w, "web UI not built", http.StatusNotFound)
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

// writable answers 409 chat_in_trash to a route that would change its chat while the chat is in the trash
// (issue #76). The manager refuses the same work on its own paths; this is the one answer for the UI.
func (s *Server) writable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trashed, err := s.M.Trashed(r.Context(), r.PathValue("id"))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			fail(w, err)
			return
		}
		if trashed {
			writeJSON(w, http.StatusConflict, map[string]string{"error": chat.ErrTrashed.Error(), "code": "chat_in_trash"})
			return
		}
		next(w, r)
	}
}

// approvalWritable: an approval of a chat in the trash cannot be decided (409 chat_in_trash). It is rejected
// when the chat goes into the trash, so this only answers a late click.
func (s *Server) approvalWritable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, err := s.owners().ApprovalChat(r.Context(), r.PathValue("id"))
		if err == nil {
			trashed, err := s.M.Trashed(r.Context(), chatID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				fail(w, err)
				return
			}
			if trashed {
				writeJSON(w, http.StatusConflict, map[string]string{"error": chat.ErrTrashed.Error(), "code": "chat_in_trash"})
				return
			}
		}
		next(w, r)
	}
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
	case errors.Is(err, chat.ErrExperimentNotAllowed):
		return http.StatusForbidden
	case errors.Is(err, chat.ErrUnknownModel), errors.Is(err, chat.ErrUnknownVariant), errors.Is(err, chat.ErrVariantFixed), errors.Is(err, chat.ErrInvalid),
		errors.Is(err, chat.ErrExperimentNeedsDelegation), errors.Is(err, chat.ErrExperimentProtection), errors.Is(err, chat.ErrExperimentTrace):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func fail(w http.ResponseWriter, err error) {
	// Trash (issue #76): the code tells the UI what to offer.
	if errors.Is(err, chat.ErrTrashed) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "chat_in_trash"})
		return
	}
	if errors.Is(err, chat.ErrNotInTrash) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "not_in_trash"})
		return
	}
	// Model switch blocked: include the numbers so that the UI can ask whether to compact first.
	var tooLarge *chat.ContextTooLargeError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "context_too_large", "details": tooLarge})
		return
	}
	code := errCode(err)
	if code >= 500 {
		slog.Error("API error", "error", err)
	}
	writeErr(w, code, err.Error())
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Cat.Models())
}

// platformStatus reports the binding to the platform: setup, whether the API answers, and the last token
// exchange among the user's own chats. Read-only; the probe goes out without a token.
func (s *Server) platformStatus(w http.ResponseWriter, r *http.Request) {
	if s.Platform == nil {
		writeJSON(w, 200, map[string]any{"configured": false})
		return
	}
	out := map[string]any{"configured": true}
	in := s.Platform.Info()
	out["api_url"], out["login"], out["client_id"], out["token_exchange"] = in.APIURL, in.Login, in.ClientID, in.TokenExchange
	if in.Account != "" {
		out["account"] = in.Account
	}
	out["probe"] = s.Platform.Probe(r.Context())
	ctx := r.Context()
	if last, ok := s.Platform.LastExchange(func(id string) bool { ok, err := s.owns(ctx, id); return err == nil && ok }); ok {
		out["last_exchange"] = last
	}
	writeJSON(w, 200, out)
}

// variants lists every combination of bindings; active marks the one new chats get (AGW_TOOLSETS).
// The others remain for chats stored with them.
func (s *Server) variants(w http.ResponseWriter, r *http.Request) {
	active := s.M.Options().Toolsets.Key()
	out := make([]worker.VariantInfo, 0, len(worker.Variants))
	for _, v := range worker.Variants {
		v.Active = v.ID == active
		out = append(out, v)
	}
	writeJSON(w, 200, out)
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
		// Subagents at the same time per chat, fixed for the service (issue #24). The two older keys
		// carry the same value for clients that still read them.
		"max_subagents":         o.MaxSubagents,
		"max_subagents_default": o.MaxSubagents,
		"max_subagents_limit":   o.MaxSubagents,
		"workspace_max_mb":      workspaceMaxMB(o.WorkspaceMaxBytes),
		"bg_wakes_per_hour":     o.BgWakesPerHour,
		"bg_keepalive_s":        int(o.BgKeepAlive.Seconds()),
		"auto_turns_max":        o.AutoTurnsMax,
		// Tools whose execution is evidenced at the socket (E9, L6): the UI matches against them
		// instead of keeping the list itself.
		"executed_tools": chat.ExecutedToolNames(),
		// Bindings of every new chat (AGW_TOOLSETS, issue #29): key, bindings, label and the union
		// of their tools. There is no choice per chat.
		"toolsets": worker.Info(o.Toolsets),
	})
}

// workspaceMaxMB: limit of the /workspace backup in MB, 0 = no backup.
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
	// execution sandbox of the slot (E9)
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
			// The user does not see identifiers and titles of foreign chats (oidc).
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
		"slots":    out,
		"targets":  s.Pool.Targets(),
		"toolsets": s.M.Options().Toolsets.Key(), // key kept warm; other keys only on demand
		"totals":   map[string]any{"cost": cost, "tokens": tok, "chats_active": active},
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
		writeErr(w, 400, "invalid JSON")
		return
	}
	req.Owner, req.OwnerName = "", ""
	if ExpKeyFrom(r.Context()) && req.Experiment == nil {
		// X1.3: the experiment key creates experiment chats and nothing else.
		writeErr(w, http.StatusForbidden, "the experiment key only creates experiment chats")
		return
	}
	if s.OIDC != nil {
		u, ok := UserFrom(r.Context())
		if !ok || u.Sub == "" {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		req.Owner = u.Sub
		req.OwnerName = u.Username // X0.2: AGW_EXPERIMENT_USERS names preferred_username
	}
	c, err := s.M.Create(r.Context(), req)
	if err != nil {
		if errors.Is(err, chat.ErrNoSlot) {
			writeErr(w, http.StatusServiceUnavailable, "No free slot in the pool, please wait a moment.")
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
	subMsgs, err8 := s.M.SubagentMessages(ctx, id)
	queue, err6 := s.M.Queue(ctx, id)
	bg, err7 := s.M.BackgroundTasks(ctx, id)
	if err := errors.Join(err1, err2, err3, err4, err5, err6, err7, err8); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"chat": c, "messages": msgs, "artifacts": arts, "approvals": aps, "socket_calls": calls, "subagent_entries": subs, "subagent_runs": subRuns, "subagent_messages": subMsgs, "queue": queue, "queue_delivered": s.M.DeliveredQueue(id), "background": bg})
}

// subagentRuns answers the short list of a chat's subagent runs (issue #60) from the database alone: it does not
// call View, Resume or anything else that could wake the chat or take a slot.
func (s *Server) subagentRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// token mode has no owner check: an unknown chat is 404 here as on the other routes, not an empty list
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	runs, err := s.M.SubagentRunSummaries(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, runs)
}

// subagentMessage takes a message of the user to a subagent (issue #81) and answers 202 with the stored message;
// its status follows over SSE (subagent_message). Refusals carry a code for the UI.
func (s *Server) subagentMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	msg, err := s.M.SendSubagentMessage(r.Context(), r.PathValue("id"), r.PathValue("run"), req.Text)
	if err != nil {
		subagentMessageFail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": msg, "mode": msg.Mode})
}

// subagentMessageFail maps the refusals of a message to a subagent to status and code.
func subagentMessageFail(w http.ResponseWriter, err error) {
	code := ""
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, chat.ErrMessageRate):
		status, code = http.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, chat.ErrSubagentStarting):
		status, code = http.StatusConflict, "run_starting"
	case errors.Is(err, chat.ErrSubagentForeground):
		status, code = http.StatusConflict, "foreground_run"
	case errors.Is(err, chat.ErrMessagePending):
		status, code = http.StatusConflict, "message_pending"
	case errors.Is(err, chat.ErrSubagentLimit):
		status, code = http.StatusConflict, "subagent_limit"
	case errors.Is(err, chat.ErrNotResumable):
		status, code = http.StatusConflict, "not_resumable"
	case errors.Is(err, chat.ErrSubagentsUnavailable):
		status, code = http.StatusConflict, "subagents_unavailable"
	case errors.Is(err, chat.ErrTrashed):
		status, code = http.StatusConflict, "chat_in_trash"
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, chat.ErrInvalid):
		status = http.StatusBadRequest
	default:
		fail(w, err)
		return
	}
	body := map[string]string{"error": err.Error()}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}

// subagentStop stops a single running subagent (issue #120, L3) and answers 202 with the run and the recorded reason;
// the answer of pi-subagents arrives later and does not change it. Refusals carry a code for the UI.
func (s *Server) subagentStop(w http.ResponseWriter, r *http.Request) {
	run, err := s.M.StopSubagent(r.Context(), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		subagentStopFail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

// subagentStopFail maps the refusals of a stop to status and code. The texts the user sees are the frontend's
// (src/agent/subagents.ts, subagentStopErrorText); the code is the contract.
func subagentStopFail(w http.ResponseWriter, err error) {
	code := ""
	status := http.StatusInternalServerError
	var relay *chat.SubagentRelayError
	switch {
	case errors.As(err, &relay):
		// pi-subagents or the bridge refused: its code travels (not_found, invalid_state, execution_failed …)
		code, status = relay.Code, http.StatusConflict
		if relay.Code == "not_found" {
			status = http.StatusNotFound
		}
	case errors.Is(err, chat.ErrSubagentNotRunning):
		status, code = http.StatusConflict, "not_running"
	case errors.Is(err, chat.ErrTrashed):
		status, code = http.StatusConflict, "chat_in_trash"
	case errors.Is(err, chat.ErrSubagentsUnavailable):
		status, code = http.StatusConflict, "subagents_unavailable"
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, chat.ErrInvalid):
		status = http.StatusBadRequest
	default:
		fail(w, err)
		return
	}
	body := map[string]string{"error": err.Error()}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text        string          `json:"text"`
		Attachments []string        `json:"attachments"`
		Context     json.RawMessage `json:"context"` // page context (issue #13), checked by chat.ParsePageContext
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if len(req.Attachments) > 20 {
		writeErr(w, 400, "at most 20 attachments per message")
		return
	}
	pc, err := chat.ParsePageContext(req.Context)
	if err != nil {
		fail(w, err)
		return
	}
	res, err := s.M.SendMessage(r.Context(), r.PathValue("id"), req.Text, req.Attachments, pc)
	if err != nil {
		sendFail(w, err)
		return
	}
	writeSend(w, res)
}

func sendFail(w http.ResponseWriter, err error) {
	if errors.Is(err, chat.ErrNoSlot) {
		writeErr(w, http.StatusServiceUnavailable, "No free slot in the pool, please wait a moment.")
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
			writeErr(w, http.StatusConflict, "The agent is working; the messages go out when the run ends.")
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
			writeErr(w, http.StatusConflict, "The message has already been handed to the agent.")
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
		writeErr(w, 400, `expected {"enabled": true|false}`)
		return
	}
	c, err := s.M.SetInternetByUser(r.Context(), r.PathValue("id"), *req.Enabled)
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
		writeErr(w, 400, `expected {"model": "provider/model", "compact_first": false}`)
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
		writeErr(w, 400, `expected {"level": "off|minimal|low|medium|high|xhigh|max"}`)
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
		writeErr(w, 400, `expected {"enabled": true|false}`)
		return
	}
	c, err := s.M.SetAutoCompact(r.Context(), r.PathValue("id"), *req.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

// subagentLimitGone: the limit per chat was removed (issue #24); the route answers 410 so that older clients
// get a clear message instead of a 404 that looks like an unknown chat.
func (s *Server) subagentLimitGone(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusGone, fmt.Sprintf("the subagent limit is fixed for the service (%d at the same time) and cannot be changed per chat", s.M.Options().MaxSubagents))
}

func (s *Server) llmCalls(w http.ResponseWriter, r *http.Request) {
	calls, err := s.M.LLMCalls(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, calls)
}

// toolExecutions: matching requested (proxy) ↔ executed (orchestrator) per toolCallId (E9).
// background: the chat's background tasks (running ones with the slot's current state).
func (s *Server) background(w http.ResponseWriter, r *http.Request) {
	list, err := s.M.BackgroundTasks(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

// stopBackground: stop a background task at the user's request; 409 if it is not running.
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
		writeErr(w, 400, `expected {"command": "/…"}`)
		return
	}
	res, err := s.M.RunCommand(r.Context(), r.PathValue("id"), req.Command)
	if err != nil {
		if errors.Is(err, chat.ErrNoSlot) {
			writeErr(w, http.StatusServiceUnavailable, "No free slot in the pool, please wait a moment.")
			return
		}
		if errors.Is(err, chat.ErrRunning) {
			writeErr(w, http.StatusConflict, "The agent is working; /compact only works afterwards.")
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
	var src ArtifactOpener = s.M
	if s.Artifacts != nil {
		src = s.Artifacts
	}
	rc, size, err := src.OpenArtifact(r.Context(), r.PathValue("id"), kind, name)
	if err != nil {
		fail(w, err)
		return
	}
	defer rc.Close()
	if r.URL.Query().Get("inline") == "1" {
		serveInline(w, rc, size, name)
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	_, _ = io.Copy(w, rc)
}

// serveInline shows a stored file in the file preview of the UI (inline=1). The kind comes from
// artifacts.PreviewKind, decided from the bytes: images and PDFs under a strict policy, tables and text as text/plain.
// HTML and SVG are never served as documents, only as text. Anything else answers 415.
func serveInline(w http.ResponseWriter, rc io.Reader, size int64, name string) {
	head := make([]byte, artifacts.PreviewHeadSize)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		http.Error(w, "could not read the file", http.StatusInternalServerError)
		return
	}
	head = head[:n]
	var ct, csp string
	switch artifacts.PreviewKind(name, head) {
	case artifacts.PreviewImage:
		ct, csp = artifacts.ImageType(head), "sandbox; default-src 'none'"
	case artifacts.PreviewPDF:
		ct, csp = "application/pdf", "default-src 'none'; object-src 'self'; plugin-types application/pdf"
	case artifacts.PreviewTable, artifacts.PreviewText:
		ct, csp = "text/plain; charset=utf-8", "sandbox; default-src 'none'"
	default:
		http.Error(w, "no preview for this file type", http.StatusUnsupportedMediaType)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", fmt.Sprint(size))
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(head)
	_, _ = io.Copy(w, rc)
}

// image serves an image the agent shows in a response. The type comes
// from the magic bytes (PNG, JPEG, GIF, WebP only), never from the file extension.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, msg := q.Get("path"), q.Get("msg")
	if p == "" || msg == "" {
		writeErr(w, 400, "expected ?path=<path in the sandbox>&msg=<identifier of the response>")
		return
	}
	var src ImageOpener = s.M
	if s.Images != nil {
		src = s.Images
	}
	im, rc, err := src.OpenImage(r.Context(), r.PathValue("id"), msg, p)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store") // possibly available later
		fail(w, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", im.ContentType)
	h.Set("Content-Length", fmt.Sprint(im.Size))
	h.Set("X-Content-Type-Options", "nosniff")
	// Per (response, path) the first backup applies; the content no longer changes.
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(im.Path)}))
	_, _ = io.Copy(w, rc)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	max := s.M.Options().ArtifactMaxBytes
	r.Body = http.MaxBytesReader(w, r.Body, 20*max+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "upload unreadable: "+err.Error())
		return
	}
	var out []store.Artifact
	for _, fh := range r.MultipartForm.File["file"] {
		if fh.Size > max {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s is larger than %d MB", fh.Filename, max>>20))
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
		writeErr(w, 400, "no file in field file")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// removeInput takes back an input the user uploaded but did not send with a message (issue L4/#121): 204, or
// 404 for an unknown name, or 409 "already_sent" for a file that went with a message.
func (s *Server) removeInput(w http.ResponseWriter, r *http.Request) {
	err := s.M.RemoveInput(r.Context(), r.PathValue("id"), r.PathValue("name"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, chat.ErrAlreadySent):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "already_sent"})
	default:
		fail(w, err)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.View(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not possible")
		return
	}
	ch, cancel := s.M.Subscribe(id)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, ": connected\n\n")
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
	if chatID == "" {
		if a, err = s.ownApprovals(ctx, a, map[string]bool{}); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, 200, a)
}

// ownApprovals keeps the approvals of the user's chats (all of them in token mode). mine caches the
// ownership per chat ID and may be shared across calls of one request.
func (s *Server) ownApprovals(ctx context.Context, a []store.Approval, mine map[string]bool) ([]store.Approval, error) {
	if s.OIDC == nil {
		return a, nil
	}
	own := make([]store.Approval, 0, len(a))
	for _, ap := range a {
		ok, seen := mine[ap.ChatID]
		if !seen {
			var err error
			if ok, err = s.owns(ctx, ap.ChatID); err != nil {
				return nil, err
			}
			mine[ap.ChatID] = ok
		}
		if ok {
			own = append(own, ap)
		}
	}
	return own, nil
}

// eventsRetryMs: reconnect delay the stream across chats suggests to EventSource (SSE "retry").
const eventsRetryMs = 2000

// allEvents is the stream across the user's chats (GET /api/events): first the pending approvals as
// one "approvals" event (the complete state, also after every reconnect), then every new or decided
// approval of the user's chats as "approval". Other users' approvals never appear in oidc mode.
func (s *Server) allEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not possible")
		return
	}
	// Subscribe before reading the snapshot, so nothing decided in between is lost; an event that is
	// already part of the snapshot only repeats its state.
	ch, cancel := s.M.SubscribeAll()
	defer cancel()
	mine := map[string]bool{}
	pending, err := s.M.Approvals(ctx, store.ApprovalPending, "")
	if err == nil {
		pending, err = s.ownApprovals(ctx, pending, mine)
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_, _ = fmt.Fprintf(w, "retry: %d\n: connected\n\n", eventsRetryMs)
	send := func(ev chat.Event) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	send(chat.Event{Kind: "approvals", Data: pending})
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			own, seen := mine[ev.ChatID]
			if !seen {
				if own, err = s.owns(ctx, ev.ChatID); err != nil {
					return // the client reconnects and gets a fresh snapshot
				}
				mine[ev.ChatID] = own
			}
			if own {
				send(ev.Event)
			}
		}
	}
}

// Limits of GET /api/activity: page size by default and at most.
const (
	activityDefaultLimit = 100
	activityMaxLimit     = 500
)

// activity lists the platform calls of the user's chats across all chats, newest first, with a summary of
// the period (API.md, Activity across chats); kind=internet|all adds or selects the internet switches. In oidc mode the store filters by the owner, so another user's
// calls never leave the database; a foreign chat in ?chat= gives an empty page, like an unknown one.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ActivityFilter{Kind: q.Get("kind"), ChatID: q.Get("chat"), Outcome: q.Get("outcome"), Limit: activityDefaultLimit}
	if v := q.Get("call"); v != "" {
		// Same shape as a tool call id in the socket log (sock.validToolCallID): the step details of a chat link
		// here with the id of their step (issue L6/#123).
		if !validToolCallParam(v) {
			writeErr(w, http.StatusBadRequest, "call: a tool call id (at most 256 bytes, no control characters)")
			return
		}
		f.ToolCall = v
	}
	if f.Kind != "" && !slices.Contains(store.ActivityKinds, f.Kind) {
		writeErr(w, http.StatusBadRequest, "kind: one of "+strings.Join(store.ActivityKinds, ", "))
		return
	}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeErr(w, http.StatusBadRequest, name+": expected an RFC 3339 time such as 2026-10-06T00:00:00Z")
				return
			}
			*dst = t
		}
	}
	if f.Outcome != "" && !slices.Contains(store.Outcomes, f.Outcome) {
		writeErr(w, http.StatusBadRequest, "outcome: one of "+strings.Join(store.Outcomes, ", "))
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit: a positive number")
			return
		}
		f.Limit = min(n, activityMaxLimit)
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "before: a positive call id (next_before of the previous page)")
			return
		}
		f.Before = n
	}
	if s.OIDC != nil {
		u, ok := UserFrom(r.Context())
		if !ok || u.Sub == "" {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		f.Owner = &u.Sub
	}
	page, err := s.M.Activity(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// validToolCallParam mirrors sock.validToolCallID for the ?call= filter of the activity: a tool call id is
// printable, valid UTF-8 and at most 256 bytes.
func validToolCallParam(id string) bool {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approve *bool `json:"approve"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Approve == nil {
		writeErr(w, 400, `expected {"approve": true|false}`)
		return
	}
	a, err := s.M.Decide(r.Context(), r.PathValue("id"), *req.Approve)
	if err != nil {
		// The platform changed since the approval was proposed (issue L2/#119): the approval stays open and carries
		// the newly read summary next to the one as proposed, so the card shows both and the next click decides.
		if errors.Is(err, chat.ErrSummaryChanged) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "summary_changed", "approval": a})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

// ParseSubnets reads a comma-separated list of CIDR ranges.
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

// trashOwner is the owner whose trash is listed or emptied: the user with OIDC, all chats in token mode.
func (s *Server) trashOwner(r *http.Request) *string {
	if s.OIDC == nil {
		return nil
	}
	u, _ := UserFrom(r.Context())
	sub := u.Sub
	return &sub
}

// trashChat moves a chat into the trash (idempotent).
func (s *Server) trashChat(w http.ResponseWriter, r *http.Request) {
	c, err := s.M.Trash(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

// restoreChat takes a chat out of the trash; it does not wake the chat.
func (s *Server) restoreChat(w http.ResponseWriter, r *http.Request) {
	c, err := s.M.Restore(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, c)
}

// deleteChat deletes a chat for good; only from the trash (409 not_in_trash otherwise).
func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	if err := s.M.DeletePermanently(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listTrash lists the chats of the user in the trash, most recently moved first. purge_at is set only with a
// retention (AGW_TRASH_RETENTION_DAYS above 0).
func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	items, err := s.M.TrashList(r.Context(), s.trashOwner(r))
	if err != nil {
		fail(w, err)
		return
	}
	days := s.M.Options().TrashRetentionDays
	type entry struct {
		store.TrashItem
		PurgeAt *time.Time `json:"purge_at,omitempty"`
	}
	out := make([]entry, 0, len(items))
	for _, it := range items {
		e := entry{TrashItem: it}
		if days > 0 {
			p := it.TrashedAt.Add(time.Duration(days) * 24 * time.Hour)
			e.PurgeAt = &p
		}
		out = append(out, e)
	}
	writeJSON(w, 200, map[string]any{"retention_days": days, "chats": out})
}

// emptyTrash deletes every chat of the user in the trash for good. A failing chat does not stop the others; then
// the answer is 500 with the number that went.
func (s *Server) emptyTrash(w http.ResponseWriter, r *http.Request) {
	n, err := s.M.EmptyTrash(r.Context(), s.trashOwner(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "deleted": n})
		return
	}
	writeJSON(w, 200, map[string]int{"deleted": n})
}

// Package sock bedient den Unix-Socket eines Platzes (E3, E4). Welcher Chat
// gemeint ist, ergibt sich allein aus dem Platz, an dessen Socket die Anfrage
// eingeht; Angaben des Agenten spielen dafür keine Rolle.
//
//	POST /artifacts?name=N   Upload, wartet auf Bestätigung durch den Nutzer
//	GET  /artifacts          Artefakte des Chats
//	GET  /artifacts/{name}   Download (?kind=input|output)
//	POST /internet           Internetzugang erbitten, wartet auf Bestätigung
//	POST /platform/{tool}    Werkzeug der Plattform-Anbindung (agw-platform); schreibend mit Bestätigung
//	POST /mcp                MCP (Streamable HTTP, zustandslos)
package sock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agw/internal/artifacts"
	"agw/internal/execproto"
	"agw/internal/platform"
	"agw/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const SocketName = "agw.sock"

type UploadResult struct {
	Status  string `json:"status"` // approved | rejected
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Message string `json:"message,omitempty"`
}

type Backend interface {
	ChatForSlot(slotID string) string
	Upload(ctx context.Context, chatID, slotID, via, name string, size int64, sha string, body io.Reader) (UploadResult, error)
	ListArtifacts(ctx context.Context, chatID string) ([]store.Artifact, error)
	OpenArtifact(ctx context.Context, chatID, kind, name string) (io.ReadCloser, int64, error)
	RequestInternet(ctx context.Context, chatID, slotID, via, reason string) (UploadResult, error)
	LogCall(slotID, chatID, via, op, detail, result string)
}

// PlatformBackend führt Aufrufe der Agri-Gaia-Plattform aus (Manager): GET direkt,
// alles andere erst nach Bestätigung durch den Nutzer. Setzt ein Backend es nicht
// um, melden die Werkzeuge „nicht eingerichtet“.
type PlatformBackend interface {
	PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error)
}

// PlatformPrechecker prüft einen Aufruf vorab gegen die Delegation (vor dem Lesen von Dateien).
type PlatformPrechecker interface {
	PlatformPrecheck(ctx context.Context, chatID string, req platform.Request) (platform.Result, bool)
}

type handler struct {
	apiVia  string // Weg, unter dem der REST-Endpunkt protokolliert: api (pi) oder cli (Shell)
	slot    string
	b       Backend
	run     ToolRunner // liest Dateien für Plattform-Uploads aus der Ausführungs-Sandbox (nil: keine Uploads)
	max     int64
	mcp     http.Handler
	uploads chan struct{} // höchstens zwei Uploads je Platz gleichzeitig (Review H2)
}

// maxReason begrenzt die Begründung einer Internet-Anfrage schon am Socket.
const maxReason = 500

func truncReason(s string) string {
	if r := []rune(s); len(r) > maxReason {
		return string(r[:maxReason]) + " …"
	}
	return s
}

func newHandler(slotID string, b Backend, maxBytes int64) *handler {
	h := &handler{slot: slotID, b: b, max: maxBytes, uploads: make(chan struct{}, 2)}
	// Base64 vergrößert um 4/3; die Grenze soll für MCP dieselbe Dateigröße
	// zulassen wie für das CLI (Review M5).
	h.mcp = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return h.mcpServer() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBytes*4/3 + 64<<10})
	return h
}

// NewHandler bedient den Socket der Ausführungs-Sandbox (agw-artifact,
// agw-internet, curl --unix-socket): Artefakte, Internet und MCP.
func NewHandler(slotID string, b Backend, maxBytes int64) http.Handler {
	return NewHandlerRun(slotID, b, maxBytes, nil)
}

// NewHandlerRun ist NewHandler mit Zugriff auf die Ausführungs-Sandbox, aus der die
// Plattform-Uploads (upload_dataset, upload_model) ihre Dateien lesen.
func NewHandlerRun(slotID string, b Backend, maxBytes int64, run ToolRunner) http.Handler {
	h := newHandler(slotID, b, maxBytes)
	h.run = run
	h.apiVia = "cli" // curl --unix-socket aus der Shell
	mux := http.NewServeMux()
	mux.HandleFunc("POST /artifacts", h.upload)
	mux.HandleFunc("GET /artifacts", h.list)
	mux.HandleFunc("GET /artifacts/{name}", h.get)
	mux.HandleFunc("POST /internet", h.internet)
	mux.HandleFunc("POST /platform/{tool}", h.platform)
	mux.Handle("/mcp", h.mcp)
	return h.withPlatformAPI(mux)
}

// withPlatformAPI leitet /platform-api/… an platformAPI, bevor die ServeMux den Pfad bereinigt: Sie
// beantwortet ., .. und // sonst mit einer Weiterleitung, und der Versuch fehlte im Protokoll
// (Review 5, M4). So sieht Normalize den Pfad, wie der Agent ihn geschickt hat.
func (h *handler) withPlatformAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, PlatformAPIPrefix+"/") {
			h.platformAPI(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var errUnassigned = errors.New("Platz ist keinem Chat zugewiesen")

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *handler) chat(via, op, detail string) (string, error) {
	c := h.b.ChatForSlot(h.slot)
	if c == "" {
		h.b.LogCall(h.slot, "", via, op, detail, "abgewiesen: nicht zugewiesen")
		return "", errUnassigned
	}
	return c, nil
}

// CallerLogger protokolliert mit Sitzung und Werkzeugaufruf aus dem Kontext (Manager).
type CallerLogger interface {
	LogCallBy(ctx context.Context, slotID, chatID, via, op, detail, result string)
}

func (h *handler) logCall(ctx context.Context, chat, via, op, detail, result string) {
	if l, ok := h.b.(CallerLogger); ok {
		l.LogCallBy(ctx, h.slot, chat, via, op, detail, result)
		return
	}
	h.b.LogCall(h.slot, chat, via, op, detail, result)
}

var sessionHeaderRe = regexp.MustCompile(`^(main|[0-9a-fA-F-]{8,64}(#[0-9]{1,4})?)$`)

// callerCtx übernimmt Werkzeugaufruf und Sitzung aus den Kopfzeilen von agw-artifact (die Werte setzt
// der Orchestrator in die Umgebung jedes bash-Befehls; nur zur Anzeige, der Agent kann sie ändern).
func callerCtx(r *http.Request) context.Context {
	ctx := r.Context()
	if id := r.Header.Get("X-Agw-Tool-Call"); validToolCallID(id) {
		ctx = store.WithToolCall(ctx, id)
	}
	if s := r.Header.Get("X-Agw-Session"); sessionHeaderRe.MatchString(s) {
		ctx = store.WithSession(ctx, s)
	}
	return ctx
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	// Der Weg ergibt sich aus dem Endpunkt, nicht aus einer Angabe des Agenten
	// (Review M5): /artifacts ist der CLI-Weg, MCP kommt über /mcp.
	via := "cli"
	ctx := callerCtx(r)
	name := artifacts.SanitizeName(r.URL.Query().Get("name"))
	chat, err := h.chat(via, "upload", name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nicht zugewiesen: " + err.Error()})
		return
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ungültiger Name"})
		return
	}
	if r.ContentLength > h.max {
		h.logCall(ctx, chat, via, "upload", name, "abgewiesen: zu groß")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("Datei größer als %d MB", h.max>>20)})
		return
	}
	// Der Inhalt wird vollständig gelesen, bevor gewartet wird: Die Grenze
	// gilt auch ohne Content-Length, und die Prüfsumme steht fest.
	data, err := io.ReadAll(io.LimitReader(r.Body, h.max+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if int64(len(data)) > h.max {
		h.logCall(ctx, chat, via, "upload", name, "abgewiesen: zu groß")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("Datei größer als %d MB", h.max>>20)})
		return
	}
	res, err := h.doUpload(ctx, chat, via, name, data, r.Header.Get("X-Agw-Sha256"))
	if errors.Is(err, errBusy) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

var errBusy = errors.New("schon zwei Uploads in Arbeit; bitte nacheinander hochladen")

func (h *handler) doUpload(ctx context.Context, chat, via, name string, data []byte, sha string) (UploadResult, error) {
	select {
	case h.uploads <- struct{}{}:
		defer func() { <-h.uploads }()
	default:
		h.logCall(ctx, chat, via, "upload", name, "abgewiesen: zu viele gleichzeitig")
		return UploadResult{}, errBusy
	}
	res, err := h.b.Upload(ctx, chat, h.slot, via, name, int64(len(data)), sha, bytes.NewReader(data))
	result := res.Status
	if err != nil {
		result = "Fehler: " + err.Error()
	}
	h.logCall(ctx, chat, via, "upload", fmt.Sprintf("%s (%d Bytes)", name, len(data)), result)
	return res, err
}

func (h *handler) internet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	req.Reason = truncReason(req.Reason)
	chat, err := h.chat("cli", "internet", req.Reason)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nicht zugewiesen"})
		return
	}
	res, err := h.doInternet(callerCtx(r), chat, "cli", req.Reason)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) doInternet(ctx context.Context, chat, via, reason string) (UploadResult, error) {
	res, err := h.b.RequestInternet(ctx, chat, h.slot, via, reason)
	result := res.Status
	if err != nil {
		result = "Fehler: " + err.Error()
	}
	h.logCall(ctx, chat, via, "internet", reason, result)
	return res, err
}

// readFiles liest die Dateien eines Uploads aus der Ausführungs-Sandbox, bevor der Nutzer gefragt
// wird: Die Bestätigung nennt Größe und SHA-256 dessen, was tatsächlich hochgeladen wird.
func (h *handler) readFiles(ctx context.Context, req *platform.Request) error {
	if h.run == nil {
		return errors.New("Uploads an diesem Socket nicht möglich")
	}
	var total int64
	req.Uploads = make([]platform.Upload, 0, len(req.Files))
	for _, f := range req.Files {
		rq := execproto.Request{Op: execproto.OpRead, Path: f.Path, Max: h.max}
		if err := rq.Validate(); err != nil {
			return fmt.Errorf("%s: %v", f.Path, err)
		}
		fr, err := h.run.Run(ctx, rq, nil)
		if err != nil {
			return fmt.Errorf("%s: %v", f.Path, err)
		}
		if fr.Error != "" {
			if fr.Code == "EFBIG" {
				return fmt.Errorf("%s: größer als %d MB", f.Path, h.max>>20)
			}
			return fmt.Errorf("%s: %s", f.Path, fr.Error)
		}
		var rr execproto.ReadResult
		if err := json.Unmarshal(fr.Result, &rr); err != nil {
			return fmt.Errorf("%s: unlesbare Antwort", f.Path)
		}
		total += int64(len(rr.Data))
		if total > platform.MaxUploadBytes {
			return fmt.Errorf("Dateien zusammen größer als %d MB", platform.MaxUploadBytes>>20)
		}
		sum := sha256.Sum256(rr.Data)
		req.Uploads = append(req.Uploads, platform.Upload{Field: f.Field, Name: path.Base(f.Path), Data: rr.Data, SHA256: hex.EncodeToString(sum[:])})
	}
	return nil
}

// maxPlatformArgs begrenzt die Argumente eines Plattform-Werkzeugs (train_config, body).
const maxPlatformArgs = 1 << 20

func (h *handler) platform(w http.ResponseWriter, r *http.Request) {
	tool, ok := platform.Lookup(r.PathValue("tool"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unbekanntes Plattform-Werkzeug " + r.PathValue("tool")})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlatformArgs+1))
	if err != nil || len(raw) > maxPlatformArgs {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Argumente zu groß"})
		return
	}
	chat, err := h.chat("cli", "platform", tool.Name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nicht zugewiesen"})
		return
	}
	res, err := h.doPlatform(callerCtx(r), chat, "cli", tool, raw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// doPlatform baut den Aufruf aus der Werkzeugtabelle, lässt ihn vom Backend
// ausführen (mit Bestätigung, wenn er schreibt) und protokolliert ihn.
func (h *handler) doPlatform(ctx context.Context, chat, via string, tool platform.Tool, raw json.RawMessage) (platform.Result, error) {
	req, err := tool.Build(raw)
	if err != nil {
		h.logCall(ctx, chat, via, "platform", tool.Name, "abgewiesen: "+err.Error())
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	return h.runPlatform(ctx, chat, via, req)
}

// runPlatform führt einen gebauten Aufruf über das Backend aus (Delegation, Bestätigung, Token) und
// protokolliert ihn; gemeinsam für Werkzeuge (MCP, agw-platform) und den REST-Endpunkt.
func (h *handler) runPlatform(ctx context.Context, chat, via string, req platform.Request) (platform.Result, error) {
	pb, ok := h.b.(PlatformBackend)
	if !ok {
		h.logCall(ctx, chat, via, "platform", req.String(), "nicht eingerichtet")
		return platform.Result{Status: "error", Message: platform.ErrNotConfigured.Error()}, nil
	}
	if len(req.Files) > 0 {
		if pc, ok := h.b.(PlatformPrechecker); ok {
			if res, allowed := pc.PlatformPrecheck(ctx, chat, req); !allowed {
				h.logCall(ctx, chat, via, "platform", req.String(), "übergriff abgewiesen: "+res.Message)
				return res, nil
			}
		}
		if err := h.readFiles(ctx, &req); err != nil {
			h.logCall(ctx, chat, via, "platform", req.String(), "abgewiesen: "+err.Error())
			return platform.Result{Status: "error", Message: err.Error()}, nil
		}
	}
	res, err := pb.PlatformCall(ctx, chat, h.slot, via, req)
	result := res.Status
	if res.HTTPStatus != 0 {
		result = fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	}
	switch {
	case res.Status == "denied":
		result = "übergriff abgewiesen: " + res.Message
	case res.Violation != "":
		result += " · übergriff, nur protokolliert: " + res.Violation
	}
	if err != nil {
		result = "Fehler: " + err.Error()
	}
	h.logCall(ctx, chat, via, "platform", req.String(), result)
	return res, err
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	chat, err := h.chat("cli", "list", "")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nicht zugewiesen"})
		return
	}
	items, err := h.b.ListArtifacts(r.Context(), chat)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.logCall(callerCtx(r), chat, "cli", "list", "", fmt.Sprintf("%d Einträge", len(items)))
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	name := artifacts.SanitizeName(r.PathValue("name"))
	kind := r.URL.Query().Get("kind")
	if kind != store.KindInput {
		kind = store.KindOutput
	}
	chat, err := h.chat("cli", "get", name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nicht zugewiesen"})
		return
	}
	rc, size, err := h.b.OpenArtifact(r.Context(), chat, kind, name)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		h.logCall(callerCtx(r), chat, "cli", "get", kind+"/"+name, "nicht gefunden")
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	defer rc.Close()
	h.logCall(callerCtx(r), chat, "cli", "get", kind+"/"+name, "ok")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, rc)
}

// --- MCP ---

type pingIn struct{}
type listIn struct{}
type internetIn struct {
	Reason string `json:"reason" jsonschema:"Begründung für den Nutzer: wofür wird Internet gebraucht?"`
}

type uploadIn struct {
	Name          string `json:"name" jsonschema:"Name des Artefakts"`
	ContentBase64 string `json:"content_base64" jsonschema:"Inhalt der Datei, base64-kodiert"`
}

func textResult(isErr bool, format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: isErr, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func (h *handler) mcpServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agw-orchestrator", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "ping", Description: "Testwerkzeug ohne Nebenwirkung: meldet Zeit, Chat und Platz. Prüft den Weg vom Agenten über den Socket zum Orchestrator."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ pingIn) (*mcp.CallToolResult, any, error) {
			chat, err := h.chat("mcp", "ping", "")
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			h.b.LogCall(h.slot, chat, "mcp", "ping", "", "ok")
			return textResult(false, "pong · Zeit %s · Chat %s · Platz %s", time.Now().Format(time.RFC3339), chat, h.slot), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_artifacts", Description: "Listet die Artefakte dieses Chats (Eingaben des Nutzers und Ergebnisse)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, any, error) {
			chat, err := h.chat("mcp", "list", "")
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			items, err := h.b.ListArtifacts(ctx, chat)
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			h.b.LogCall(h.slot, chat, "mcp", "list", "", fmt.Sprintf("%d Einträge", len(items)))
			if len(items) == 0 {
				return textResult(false, "(keine Artefakte in diesem Chat)"), nil, nil
			}
			var buf bytes.Buffer
			for _, a := range items {
				fmt.Fprintf(&buf, "%s\t%s\t%d Bytes\n", a.Kind, a.Name, a.Size)
			}
			return textResult(false, "%s", buf.String()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "upload_artifact", Description: "Legt eine Datei als Artefakt dieses Chats ab. Der Nutzer muss den Upload bestätigen; der Aufruf wartet auf die Entscheidung."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in uploadIn) (*mcp.CallToolResult, any, error) {
			name := artifacts.SanitizeName(in.Name)
			chat, err := h.chat("mcp", "upload", name)
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
			if err != nil || name == "" {
				return textResult(true, "ungültiger Name oder Inhalt"), nil, nil
			}
			if int64(len(data)) > h.max {
				h.b.LogCall(h.slot, chat, "mcp", "upload", name, "abgewiesen: zu groß")
				return textResult(true, "Datei größer als %d MB", h.max>>20), nil, nil
			}
			res, err := h.doUpload(ctx, chat, "mcp", name, data, "")
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			if res.Status == "approved" {
				return textResult(false, "bestätigt: Artefakt %q gespeichert (%d Bytes, sha256 %s)", res.Name, res.Size, res.SHA256), nil, nil
			}
			msg := res.Message
			if msg == "" {
				msg = "vom Nutzer abgelehnt"
			}
			return textResult(false, "abgelehnt: Artefakt %q wurde nicht gespeichert (%s)", res.Name, msg), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "request_internet", Description: "Bittet den Nutzer um Internetzugang für diese Sandbox (standardmäßig aus). Mit Begründung aufrufen; der Aufruf wartet auf die Entscheidung des Nutzers."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in internetIn) (*mcp.CallToolResult, any, error) {
			in.Reason = truncReason(in.Reason)
			chat, err := h.chat("mcp", "internet", in.Reason)
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			res, err := h.doInternet(ctx, chat, "mcp", in.Reason)
			if err != nil {
				return textResult(true, "Fehler: %v", err), nil, nil
			}
			if res.Status == "approved" {
				return textResult(false, "bestätigt: %s", res.Message), nil, nil
			}
			return textResult(false, "abgelehnt: %s", res.Message), nil, nil
		})
	for _, t := range platform.Tools {
		s.AddTool(&mcp.Tool{Name: t.MCPName(), Description: t.Desc, InputSchema: t.Schema()},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				chat, err := h.chat("mcp", "platform", t.Name)
				if err != nil {
					return textResult(true, "Fehler: %v", err), nil
				}
				res, err := h.doPlatform(ctx, chat, "mcp", t, req.Params.Arguments)
				if err != nil {
					return textResult(true, "Fehler: %v", err), nil
				}
				return textResult(res.Status == "error" || res.Status == "denied", "%s", res.Text()), nil
			})
	}
	return s
}

// Server ist ein HTTP-Server an einem Unix-Socket.
type Server struct {
	srv  *http.Server
	path string
}

// Listen legt dir/agw.sock an. Der Socket ist für alle Nutzer verbindbar, weil
// pi in der Sandbox als eigener Nutzer läuft; abgegrenzt wird über das
// Verzeichnis, das nur diese eine Sandbox eingebunden hat.
func Listen(dir string, h http.Handler) (*Server, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, SocketName)
	_ = os.Remove(p)
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Server{srv: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}, path: p}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	_ = os.Remove(s.path)
	return nil
}

// --- REST-Endpunkt (Schritt 2: REST-Variante) ---

// PlatformAPIPrefix: Unter diesem Pfad bildet der Socket die REST-API der Plattform nach. Der Agent
// ruft sie ohne Anmeldung auf; Prüfung, Bestätigung und Token übernimmt der Autorisierungsdienst.
const PlatformAPIPrefix = "/platform-api"

// platformAPI nimmt einen HTTP-Aufruf in der Form der Plattform-API entgegen, baut daraus einen
// eigenen Aufruf (nichts wird roh durchgereicht, auch keine Kopfzeilen) und gibt Status, Location und
// Körper zurück. Uploads (multipart) gehen nur über upload_dataset und upload_model.
func (h *handler) platformAPI(w http.ResponseWriter, r *http.Request) {
	via := h.apiVia
	if via == "" {
		via = "api"
	}
	fail := func(code int, msg string) {
		writeJSON(w, code, map[string]string{"error": msg})
	}
	// Prozentkodierung lässt der Autorisierungsdienst nicht zu: Was er prüft, soll genau das sein,
	// was die Plattform liest (keine zweite Dekodierung, keine Parserdifferenz).
	// Einzige Ausnahme ist %20: Ohne sie wären Pfade mit Leerzeichen (/train/config/Torchvision/Mask
	// R-CNN) in der REST-Variante unerreichbar, über MCP und CLI aber nicht (Review 5, M5).
	if pathPart, _, _ := strings.Cut(r.RequestURI, "?"); r.URL.RawPath != "" || strings.Contains(strings.ReplaceAll(pathPart, "%20", ""), "%") {
		fail(http.StatusBadRequest, "Prozentkodierung im Pfad ist nicht erlaubt")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, PlatformAPIPrefix)
	if path == "" {
		path = "/"
	}
	q := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) != 1 {
			fail(http.StatusBadRequest, fmt.Sprintf("Abfrageparameter %q mehrfach: nicht unterstützt", k))
			return
		}
		q[k] = vs[0]
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlatformArgs+1))
	if err != nil || len(raw) > maxPlatformArgs {
		fail(http.StatusRequestEntityTooLarge, "Körper zu groß (höchstens 1 MiB)")
		return
	}
	if ct := r.Header.Get("Content-Type"); len(bytes.TrimSpace(raw)) > 0 && ct != "" && !strings.HasPrefix(ct, "application/json") {
		fail(http.StatusUnsupportedMediaType, "nur JSON-Körper; Dateien gehen über upload_dataset und upload_model")
		return
	}
	chat, err := h.chat(via, "platform", r.Method+" "+path)
	if err != nil {
		fail(http.StatusConflict, "nicht zugewiesen")
		return
	}
	ctx := callerCtx(r)
	var res platform.Result
	if path == "/_agw/paths" {
		// Verdichtetes Pfadverzeichnis wie das Werkzeug api_paths (die OpenAPI-Beschreibung ist ~100 KB).
		tool, _ := platform.Lookup("api_paths")
		args, _ := json.Marshal(map[string]string{"prefix": q["prefix"]})
		res, err = h.doPlatform(ctx, chat, via, tool, args)
	} else {
		req := platform.Request{Method: r.Method, Path: path, Body: bytes.TrimSpace(raw), Full: true}
		if len(q) > 0 {
			req.Query = q
		}
		if len(req.Body) == 0 {
			req.Body = nil
		}
		norm, nerr := platform.Normalize(req)
		if nerr != nil {
			h.logCall(ctx, chat, via, "platform", r.Method+" "+path, "abgewiesen: "+nerr.Error())
			fail(http.StatusBadRequest, nerr.Error())
			return
		}
		res, err = h.runPlatform(ctx, chat, via, norm)
	}
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	switch res.Status {
	case "denied":
		w.Header().Set("X-Agw-Outcome", "denied")
		fail(http.StatusForbidden, "verweigert vom Autorisierungsdienst: "+res.Message+" (Rechte: GET "+PlatformAPIPrefix+"/_agw/rights)")
		return
	case "rejected":
		w.Header().Set("X-Agw-Outcome", "rejected")
		fail(http.StatusForbidden, "vom Nutzer abgelehnt: "+res.Message)
		return
	}
	if res.HTTPStatus == 0 { // Prüfung der Argumente oder Plattform nicht erreichbar
		fail(http.StatusBadRequest, res.Message)
		return
	}
	if res.Location != "" {
		w.Header().Set("Location", res.Location)
	}
	if res.Truncated {
		w.Header().Set("X-Agw-Truncated", "true")
	}
	if json.Valid([]byte(res.Body)) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(res.HTTPStatus)
	_, _ = io.WriteString(w, res.Body)
}

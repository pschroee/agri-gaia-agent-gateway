// Package sock serves the Unix socket of a slot (E3, E4). Which chat is
// meant follows solely from the slot at whose socket the request arrives;
// statements by the agent play no role in this.
//
//	POST /artifacts?name=N   upload, waits for confirmation by the user
//	GET  /artifacts          artifacts of the chat
//	GET  /artifacts/{name}   download (?kind=input|output)
//	POST /internet           request internet access, waits for confirmation
//	POST /internet/off       switch internet access off (no confirmation, idempotent)
//	POST /platform/{tool}    tool of the platform binding (agw-platform); writing calls need confirmation
//	POST /mcp                MCP (Streamable HTTP, stateless)
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
	// DisableInternet switches the chat's internet off without approval (it only removes a right);
	// status InternetOff or InternetAlreadyOff.
	DisableInternet(ctx context.Context, chatID, slotID, via string) (UploadResult, error)
	LogCall(slotID, chatID, via, op, detail, result string)
}

// PlatformBackend executes calls to the Agri-Gaia platform (manager): GET directly,
// everything else only after confirmation by the user. If a backend does not
// implement it, the tools report "not configured".
type PlatformBackend interface {
	PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error)
}

// PlatformPrechecker checks a call against the delegation up front (before reading files).
type PlatformPrechecker interface {
	PlatformPrecheck(ctx context.Context, chatID string, req platform.Request) (platform.Result, bool)
}

// Statuses of DisableInternet.
const (
	InternetOff        = "off"
	InternetAlreadyOff = "already_off"
)

type handler struct {
	apiVia  string // channel under which the REST endpoint logs: api (pi) or cli (shell)
	slot    string
	b       Backend
	run     ToolRunner // reads files for platform uploads from the execution sandbox (nil: no uploads)
	max     int64
	mcp     http.Handler
	uploads chan struct{} // at most two concurrent uploads per slot (Review H2)
}

// maxReason limits the reason of an internet request already at the socket.
const maxReason = 500

func truncReason(s string) string {
	if r := []rune(s); len(r) > maxReason {
		return string(r[:maxReason]) + " …"
	}
	return s
}

func newHandler(slotID string, b Backend, maxBytes int64) *handler {
	h := &handler{slot: slotID, b: b, max: maxBytes, uploads: make(chan struct{}, 2)}
	// Base64 grows by 4/3; the limit should allow the same file size for MCP
	// as for the CLI (Review M5).
	h.mcp = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return h.mcpServer() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBytes*4/3 + 64<<10})
	return h
}

// NewHandler serves the socket of the execution sandbox (agw-artifact,
// agw-internet, curl --unix-socket): artifacts, internet and MCP.
func NewHandler(slotID string, b Backend, maxBytes int64) http.Handler {
	return NewHandlerRun(slotID, b, maxBytes, nil)
}

// NewHandlerRun is NewHandler with access to the execution sandbox, from which the
// platform uploads (upload_dataset, upload_model) read their files.
func NewHandlerRun(slotID string, b Backend, maxBytes int64, run ToolRunner) http.Handler {
	h := newHandler(slotID, b, maxBytes)
	h.run = run
	h.apiVia = "cli" // curl --unix-socket from the shell
	mux := http.NewServeMux()
	mux.HandleFunc("POST /artifacts", h.upload)
	mux.HandleFunc("GET /artifacts", h.list)
	mux.HandleFunc("GET /artifacts/{name}", h.get)
	mux.HandleFunc("POST /internet", h.internet)
	mux.HandleFunc("POST /internet/off", h.internetOff)
	mux.HandleFunc("POST /platform/{tool}", h.platform)
	mux.Handle("/mcp", h.mcp)
	return h.withPlatformAPI(mux)
}

// withPlatformAPI routes /platform-api/… to platformAPI before the ServeMux cleans the path: otherwise
// it answers ., .. and // with a redirect, and the attempt was missing from the log
// (Review 5, M4). This way Normalize sees the path as the agent sent it.
func (h *handler) withPlatformAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, PlatformAPIPrefix+"/") {
			h.platformAPI(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var errUnassigned = errors.New("slot is not assigned to any chat")

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *handler) chat(via, op, detail string) (string, error) {
	c := h.b.ChatForSlot(h.slot)
	if c == "" {
		h.b.LogCall(h.slot, "", via, op, detail, "refused: not assigned")
		return "", errUnassigned
	}
	return c, nil
}

// CallerLogger logs with session and tool call from the context (manager).
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

// callerCtx takes tool call and session from agw-artifact's headers (the orchestrator puts the values
// into the environment of every bash command; for display only, the agent can change them).
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
	// The channel follows from the endpoint, not from a statement by the agent
	// (Review M5): /artifacts is the CLI channel, MCP comes via /mcp.
	via := "cli"
	ctx := callerCtx(r)
	name := artifacts.SanitizeName(r.URL.Query().Get("name"))
	chat, err := h.chat(via, "upload", name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned: " + err.Error()})
		return
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid name"})
		return
	}
	if r.ContentLength > h.max {
		h.logCall(ctx, chat, via, "upload", name, "refused: too large")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("file larger than %d MB", h.max>>20)})
		return
	}
	// The content is read completely before waiting: the limit also
	// applies without Content-Length, and the checksum is fixed.
	data, err := io.ReadAll(io.LimitReader(r.Body, h.max+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if int64(len(data)) > h.max {
		h.logCall(ctx, chat, via, "upload", name, "refused: too large")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("file larger than %d MB", h.max>>20)})
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

var errBusy = errors.New("two uploads already in progress; please upload one after another")

func (h *handler) doUpload(ctx context.Context, chat, via, name string, data []byte, sha string) (UploadResult, error) {
	select {
	case h.uploads <- struct{}{}:
		defer func() { <-h.uploads }()
	default:
		h.logCall(ctx, chat, via, "upload", name, "refused: too many at once")
		return UploadResult{}, errBusy
	}
	res, err := h.b.Upload(ctx, chat, h.slot, via, name, int64(len(data)), sha, bytes.NewReader(data))
	result := res.Status
	if err != nil {
		result = "error: " + err.Error()
	}
	h.logCall(ctx, chat, via, "upload", fmt.Sprintf("%s (%d bytes)", name, len(data)), result)
	return res, err
}

// restVia is the channel of the plain HTTP endpoints (/internet, /platform-api): cli at the socket of the
// execution sandbox (agw-internet, curl), api at pi's socket (the tools of api.ts). It follows from the
// socket, not from a statement by the agent.
func (h *handler) restVia() string {
	if h.apiVia == "" {
		return "cli"
	}
	return h.apiVia
}

func (h *handler) internet(w http.ResponseWriter, r *http.Request) {
	via := h.restVia()
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	req.Reason = truncReason(req.Reason)
	chat, err := h.chat(via, "internet", req.Reason)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	res, err := h.doInternet(callerCtx(r), chat, via, req.Reason)
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
		result = "error: " + err.Error()
	}
	h.logCall(ctx, chat, via, "internet", reason, result)
	return res, err
}

// internetOff: POST /internet/off switches the chat's internet off. No body, no approval.
func (h *handler) internetOff(w http.ResponseWriter, r *http.Request) {
	via := h.restVia()
	chat, err := h.chat(via, "internet_off", "")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	res, err := h.doInternetOff(callerCtx(r), chat, via)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// doInternetOff switches internet off and logs the call (op internet_off, result "off" or "already off").
func (h *handler) doInternetOff(ctx context.Context, chat, via string) (UploadResult, error) {
	res, err := h.b.DisableInternet(ctx, chat, h.slot, via)
	result := strings.ReplaceAll(res.Status, "_", " ")
	if err != nil {
		result = "error: " + err.Error()
	}
	h.logCall(ctx, chat, via, "internet_off", "", result)
	return res, err
}

// readFiles reads the files of an upload from the execution sandbox before the user is asked:
// the confirmation names size and SHA-256 of what is actually uploaded.
func (h *handler) readFiles(ctx context.Context, req *platform.Request) error {
	if h.run == nil {
		return errors.New("uploads not possible at this socket")
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
				return fmt.Errorf("%s: larger than %d MB", f.Path, h.max>>20)
			}
			return fmt.Errorf("%s: %s", f.Path, fr.Error)
		}
		var rr execproto.ReadResult
		if err := json.Unmarshal(fr.Result, &rr); err != nil {
			return fmt.Errorf("%s: unreadable response", f.Path)
		}
		total += int64(len(rr.Data))
		if total > platform.MaxUploadBytes {
			return fmt.Errorf("files together larger than %d MB", platform.MaxUploadBytes>>20)
		}
		sum := sha256.Sum256(rr.Data)
		req.Uploads = append(req.Uploads, platform.Upload{Field: f.Field, Name: path.Base(f.Path), Data: rr.Data, SHA256: hex.EncodeToString(sum[:])})
	}
	return nil
}

// maxPlatformArgs limits the arguments of a platform tool (train_config, body).
const maxPlatformArgs = 1 << 20

func (h *handler) platform(w http.ResponseWriter, r *http.Request) {
	tool, ok := platform.Lookup(r.PathValue("tool"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown platform tool " + r.PathValue("tool")})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlatformArgs+1))
	if err != nil || len(raw) > maxPlatformArgs {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "arguments too large"})
		return
	}
	chat, err := h.chat("cli", "platform", tool.Name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	res, err := h.doPlatform(callerCtx(r), chat, "cli", tool, raw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// doPlatform builds the call from the tool table, has the backend execute it
// (with confirmation if it writes) and logs it.
func (h *handler) doPlatform(ctx context.Context, chat, via string, tool platform.Tool, raw json.RawMessage) (platform.Result, error) {
	req, err := tool.Build(raw)
	if err != nil {
		h.logCall(ctx, chat, via, "platform", tool.Name, "refused: "+err.Error())
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	return h.runPlatform(ctx, chat, via, req)
}

// runPlatform executes a built call through the backend (delegation, confirmation, token) and
// logs it; shared by the tools (MCP, agw-platform) and the REST endpoint.
func (h *handler) runPlatform(ctx context.Context, chat, via string, req platform.Request) (platform.Result, error) {
	pb, ok := h.b.(PlatformBackend)
	if !ok {
		h.logCall(ctx, chat, via, "platform", req.String(), "not configured")
		return platform.Result{Status: "error", Message: platform.ErrNotConfigured.Error()}, nil
	}
	if len(req.Files) > 0 {
		if pc, ok := h.b.(PlatformPrechecker); ok {
			if res, allowed := pc.PlatformPrecheck(ctx, chat, req); !allowed {
				h.logCall(ctx, chat, via, "platform", req.String(), "violation blocked: "+res.Message)
				return res, nil
			}
		}
		if err := h.readFiles(ctx, &req); err != nil {
			h.logCall(ctx, chat, via, "platform", req.String(), "refused: "+err.Error())
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
		result = "violation blocked: " + res.Message
	case res.Violation != "":
		result += " · violation, logged only: " + res.Violation
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	if res.Duration > 0 {
		ctx = store.WithDuration(ctx, res.Duration)
	}
	h.logCall(ctx, chat, via, "platform", req.String(), result)
	return res, err
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	chat, err := h.chat("cli", "list", "")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	items, err := h.b.ListArtifacts(r.Context(), chat)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.logCall(callerCtx(r), chat, "cli", "list", "", fmt.Sprintf("%d entries", len(items)))
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
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	rc, size, err := h.b.OpenArtifact(r.Context(), chat, kind, name)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		h.logCall(callerCtx(r), chat, "cli", "get", kind+"/"+name, "not found")
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
type internetOffIn struct{}
type internetIn struct {
	Reason string `json:"reason" jsonschema:"Reason for the user: what is internet needed for?"`
}

type uploadIn struct {
	Name          string `json:"name" jsonschema:"name of the artifact"`
	ContentBase64 string `json:"content_base64" jsonschema:"content of the file, base64-encoded"`
}

func textResult(isErr bool, format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: isErr, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func (h *handler) mcpServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agw-orchestrator", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "ping", Description: "Test tool without side effects: reports time, chat and slot. Checks the path from the agent through the socket to the orchestrator."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ pingIn) (*mcp.CallToolResult, any, error) {
			chat, err := h.chat("mcp", "ping", "")
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			h.b.LogCall(h.slot, chat, "mcp", "ping", "", "ok")
			return textResult(false, "pong · time %s · chat %s · slot %s", time.Now().Format(time.RFC3339), chat, h.slot), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_artifacts", Description: "Lists the artifacts of this chat (inputs from the user and results)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, any, error) {
			chat, err := h.chat("mcp", "list", "")
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			items, err := h.b.ListArtifacts(ctx, chat)
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			h.b.LogCall(h.slot, chat, "mcp", "list", "", fmt.Sprintf("%d entries", len(items)))
			if len(items) == 0 {
				return textResult(false, "(no artifacts in this chat)"), nil, nil
			}
			var buf bytes.Buffer
			for _, a := range items {
				fmt.Fprintf(&buf, "%s\t%s\t%d bytes\n", a.Kind, a.Name, a.Size)
			}
			return textResult(false, "%s", buf.String()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "upload_artifact", Description: "Stores a file as an artifact of this chat. The user has to approve the upload; the call waits for the decision."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in uploadIn) (*mcp.CallToolResult, any, error) {
			name := artifacts.SanitizeName(in.Name)
			chat, err := h.chat("mcp", "upload", name)
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
			if err != nil || name == "" {
				return textResult(true, "invalid name or content"), nil, nil
			}
			if int64(len(data)) > h.max {
				h.b.LogCall(h.slot, chat, "mcp", "upload", name, "refused: too large")
				return textResult(true, "file larger than %d MB", h.max>>20), nil, nil
			}
			res, err := h.doUpload(ctx, chat, "mcp", name, data, "")
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			if res.Status == "approved" {
				return textResult(false, "approved: artifact %q stored (%d bytes, sha256 %s)", res.Name, res.Size, res.SHA256), nil, nil
			}
			msg := res.Message
			if msg == "" {
				msg = "rejected by the user"
			}
			return textResult(false, "rejected: artifact %q was not stored (%s)", res.Name, msg), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "request_internet", Description: "Asks the user for internet access for this sandbox (off by default). Call it with a reason; the call waits for the user's decision."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in internetIn) (*mcp.CallToolResult, any, error) {
			in.Reason = truncReason(in.Reason)
			chat, err := h.chat("mcp", "internet", in.Reason)
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			res, err := h.doInternet(ctx, chat, "mcp", in.Reason)
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			if res.Status == "approved" {
				return textResult(false, "approved: %s", res.Message), nil, nil
			}
			return textResult(false, "rejected: %s", res.Message), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "disable_internet", Description: "Switches internet access for this sandbox off again, e.g. once downloads or web research are done. Needs no approval; if internet is already off, nothing changes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ internetOffIn) (*mcp.CallToolResult, any, error) {
			chat, err := h.chat("mcp", "internet_off", "")
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			res, err := h.doInternetOff(ctx, chat, "mcp")
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			return textResult(false, "%s: %s", res.Status, res.Message), nil, nil
		})
	for _, t := range platform.Tools {
		s.AddTool(&mcp.Tool{Name: t.MCPName(), Description: t.Desc, InputSchema: t.Schema()},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				chat, err := h.chat("mcp", "platform", t.Name)
				if err != nil {
					return textResult(true, "error: %v", err), nil
				}
				res, err := h.doPlatform(ctx, chat, "mcp", t, req.Params.Arguments)
				if err != nil {
					return textResult(true, "error: %v", err), nil
				}
				return textResult(res.Status == "error" || res.Status == "denied", "%s", res.Text()), nil
			})
	}
	return s
}

// Server is an HTTP server on a Unix socket.
type Server struct {
	srv  *http.Server
	path string
}

// Listen creates dir/agw.sock. The socket can be connected to by all users, because
// pi runs as its own user in the sandbox; isolation comes from the directory,
// which only this one sandbox has mounted.
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

// --- REST endpoint (step 2: REST variant) ---

// PlatformAPIPrefix: under this path the socket mirrors the platform's REST API. The agent calls it
// without logging in; the authorization service takes care of checks, confirmation and token.
const PlatformAPIPrefix = "/platform-api"

// platformAPI accepts an HTTP call in the form of the platform API, builds its own call from it
// (nothing is passed through raw, not even headers) and returns status, Location and body.
// Uploads (multipart) only go through upload_dataset and upload_model.
func (h *handler) platformAPI(w http.ResponseWriter, r *http.Request) {
	via := h.apiVia
	if via == "" {
		via = "api"
	}
	fail := func(code int, msg string) {
		writeJSON(w, code, map[string]string{"error": msg})
	}
	// The authorization service does not allow percent-encoding: what it checks should be exactly
	// what the platform reads (no second decoding, no parser differential).
	// The only exception is %20: without it, paths with spaces (/train/config/Torchvision/Mask
	// R-CNN) would be unreachable in the REST variant, but not via MCP and CLI (Review 5, M5).
	if pathPart, _, _ := strings.Cut(r.RequestURI, "?"); r.URL.RawPath != "" || strings.Contains(strings.ReplaceAll(pathPart, "%20", ""), "%") {
		fail(http.StatusBadRequest, "percent-encoding in the path is not allowed")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, PlatformAPIPrefix)
	if path == "" {
		path = "/"
	}
	q := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) != 1 {
			fail(http.StatusBadRequest, fmt.Sprintf("query parameter %q repeated: not supported", k))
			return
		}
		q[k] = vs[0]
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlatformArgs+1))
	if err != nil || len(raw) > maxPlatformArgs {
		fail(http.StatusRequestEntityTooLarge, "body too large (at most 1 MiB)")
		return
	}
	if ct := r.Header.Get("Content-Type"); len(bytes.TrimSpace(raw)) > 0 && ct != "" && !strings.HasPrefix(ct, "application/json") {
		fail(http.StatusUnsupportedMediaType, "JSON bodies only; files go through upload_dataset and upload_model")
		return
	}
	chat, err := h.chat(via, "platform", r.Method+" "+path)
	if err != nil {
		fail(http.StatusConflict, "not assigned")
		return
	}
	ctx := callerCtx(r)
	var res platform.Result
	if path == "/_agw/paths" {
		// Condensed path directory like the tool api_paths (the OpenAPI description is ~100 KB).
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
			h.logCall(ctx, chat, via, "platform", r.Method+" "+path, "refused: "+nerr.Error())
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
		fail(http.StatusForbidden, "denied by the authorization service: "+res.Message+" (rights: GET "+PlatformAPIPrefix+"/_agw/rights)")
		return
	case "rejected":
		w.Header().Set("X-Agw-Outcome", "rejected")
		fail(http.StatusForbidden, "rejected by the user: "+res.Message)
		return
	}
	if res.HTTPStatus == 0 { // argument check or platform unreachable
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

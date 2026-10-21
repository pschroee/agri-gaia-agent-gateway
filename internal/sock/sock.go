// Package sock serves the Unix socket of a slot (E3, E4). Which chat is
// meant follows solely from the slot at whose socket the request arrives;
// statements by the agent play no role in this.
//
//	POST /artifacts?name=N   send a file to the user: stored as an artifact at once, no confirmation
//	GET  /artifacts          artifacts of the chat
//	GET  /artifacts/{name}   download (?kind=input|output)
//	POST /internet           request internet access of one kind (search, domains, packages, full), waits for confirmation
//	GET  /internet           the chat's active internet grants
//	POST /internet/off       end one grant or all (no confirmation, idempotent)
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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agw/internal/artifacts"
	"agw/internal/delegation"
	"agw/internal/execproto"
	"agw/internal/netgrant"
	"agw/internal/platform"
	"agw/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const SocketName = "agw.sock"

// Statuses of an upload (issue #62: no approval any more). UploadStored: stored and sent to the user;
// UploadRejected: not stored (checksum mismatch). Requests for internet keep approved | rejected.
const (
	UploadStored   = "stored"
	UploadRejected = "rejected"
)

type UploadResult struct {
	Status  string `json:"status"` // uploads: stored | rejected; internet: approved | rejected; off: off | already_off
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Message string `json:"message,omitempty"`
	// Text (uploads): the line the agent reads (UploadResultText), the same for agw-artifact and MCP.
	Text string `json:"text,omitempty"`
	// Log is the result text for the socket log when it says more than Status (internet requests:
	// "already on", "expired"); never sent to the agent.
	Log string `json:"-"`
}

type Backend interface {
	ChatForSlot(slotID string) string
	Upload(ctx context.Context, chatID, slotID, via, name string, size int64, sha string, body io.Reader) (UploadResult, error)
	ListArtifacts(ctx context.Context, chatID string) ([]store.Artifact, error)
	OpenArtifact(ctx context.Context, chatID, kind, name string) (io.ReadCloser, int64, error)
	// RequestInternet asks the user for one internet grant (issue #175); status approved, rejected or refused.
	RequestInternet(ctx context.Context, chatID, slotID, via string, spec netgrant.Spec) (UploadResult, error)
	// DisableInternet ends one grant (grantID > 0) or all grants of the chat without approval (it only removes a
	// right); status InternetOff or InternetAlreadyOff.
	DisableInternet(ctx context.Context, chatID, slotID, via string, grantID int64) (UploadResult, error)
	LogCall(slotID, chatID, via, op, detail, result string)
}

// SubagentRelayResult is the reply of pi's extension to a relay command (issue #81): the message of the user to a
// subagent went to pi-subagents and ended as stated.
type SubagentRelayResult struct {
	Ref     string `json:"ref"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	RunID   string `json:"run_id,omitempty"`
}

// SubagentRelayBackend takes the reply to a relay command; store.ErrNotFound for an unknown or closed ref.
// A backend that does not implement it answers 404.
type SubagentRelayBackend interface {
	SubagentRelayResult(ctx context.Context, chatID string, res SubagentRelayResult) error
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

// InternetStatusBackend lists a chat's active internet grants with their counters (GET /internet); a backend that
// does not implement it answers with an empty list.
type InternetStatusBackend interface {
	ActiveGrantViews(chatID string) []store.Grant
}

// InternetRefusalsBackend lists the last refusals of a chat with their text: pip and npm print only "HTTP error 403",
// so agw-internet status is where the agent reads why (issue #183).
type InternetRefusalsBackend interface {
	RecentRefusals(chatID string) []store.WebRequest
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
	// trustedRef: this is pi's socket, whose requests come from the extensions, not from agent-controlled code
	// (E9). Only there is X-Agw-Pi-Tool-Call accepted as the reference of a platform call (X5.3).
	trustedRef bool
	// fg: the running foreground commands of the slot. On the execution sandbox's socket they give the
	// execution window that binds a CLI platform call to its tool call (X5.3).
	fg *Foreground
}

// maxReason limits the reason of an internet request already at the socket.
// RequestInternetDescription is the text of the tool request_internet (MCP and the REST binding): the agent reads it
// before it acts, so the rules are in the description (issue #175). The command line also knows packages and full.
const RequestInternetDescription = "Asks the user for internet access, only as much as the task needs. kind: search (web_search and web_extract only), " +
	"domains (https downloads from the listed host names, no wildcards). Give a short, concrete reason; the call waits for the user's decision. " +
	"Access ends by itself after a time or data limit; switch it off earlier with disable_internet."

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
	return NewHandlerRunFg(slotID, b, maxBytes, run, nil)
}

// NewHandlerRunFg is NewHandlerRun with the register of the slot's running foreground commands, which gives the
// execution window for platform calls of the CLI binding (X5.3).
func NewHandlerRunFg(slotID string, b Backend, maxBytes int64, run ToolRunner, fg *Foreground) http.Handler {
	h := newHandler(slotID, b, maxBytes)
	h.run = run
	h.fg = fg
	h.apiVia = "cli" // curl --unix-socket from the shell
	mux := http.NewServeMux()
	mux.HandleFunc("POST /artifacts", h.upload)
	mux.HandleFunc("GET /artifacts", h.list)
	mux.HandleFunc("GET /artifacts/{name}", h.get)
	mux.HandleFunc("POST /internet", h.internet)
	mux.HandleFunc("GET /internet", h.internetStatus)
	mux.HandleFunc("POST /internet/off", h.internetOff)
	mux.HandleFunc("POST /platform/{tool}", h.platform)
	mux.HandleFunc("POST /cli/exit", h.cliExit) // only here, not at pi's socket (#202)
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
func (h *handler) callerCtx(r *http.Request) context.Context {
	ctx := store.WithCallRef(callerHeader(r.Context(), r.Header), store.CallRef{Claimed: claimedToolCall(r.Header)})
	// X5.3: only pi's socket is trusted with the extension's own header. The execution sandbox can send it too, but
	// there it is agent-controlled and stays ignored (the claimed header is recorded separately).
	if h.trustedRef {
		if id := r.Header.Get("X-Agw-Pi-Tool-Call"); validToolCallID(id) {
			ref, _ := store.CallRefFrom(ctx)
			ref.Ref, ref.Source = id, "pi"
			ctx = store.WithCallRef(ctx, ref)
		}
	}
	return ctx
}

// callerHeader reads the tool call and the session from the headers a client sets: agw-artifact and agw-platform
// send X-Agw-Tool-Call and X-Agw-Session, and the pi extensions do the same on their socket requests. The values
// are for display only; the agent can change them.
func callerHeader(ctx context.Context, h http.Header) context.Context {
	if id := h.Get("X-Agw-Tool-Call"); validToolCallID(id) {
		ctx = store.WithToolCall(ctx, id)
	}
	if s := h.Get("X-Agw-Session"); sessionHeaderRe.MatchString(s) {
		ctx = store.WithSession(ctx, s)
	}
	return ctx
}

// claimedToolCall is the header the agent can set himself (X5.3: call_ref_claimed, never binding).
func claimedToolCall(h http.Header) string {
	if id := h.Get("X-Agw-Tool-Call"); validToolCallID(id) {
		return id
	}
	return ""
}

// callerExtra is callerHeader for an MCP request: the pi extension sends the tool call of the session as a header
// on POST /mcp, so that a plan proposed over MCP carries its tool_call_id like CLI and REST do (issue L2/#119).
func (h *handler) callerExtra(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		ctx = callerHeader(ctx, req.Extra.Header)
		ref, _ := store.CallRefFrom(ctx)
		ref.Claimed = claimedToolCall(req.Extra.Header)
		if h.trustedRef {
			if id := req.Extra.Header.Get("X-Agw-Pi-Tool-Call"); validToolCallID(id) {
				ref.Ref, ref.Source = id, "pi"
			}
		}
		ctx = store.WithCallRef(ctx, ref)
	}
	return ctx
}

// withExecRef binds a platform call on the execution sandbox's socket to its tool call through the execution window
// (X5.3): exactly one running foreground command is the reference; several leave the call ambiguous; none leaves it
// without one. The header is never used here.
func (h *handler) withExecRef(ctx context.Context, chat string) context.Context {
	if h.trustedRef || h.fg == nil || chat == "" {
		return ctx
	}
	ref, _ := store.CallRefFrom(ctx)
	ids := h.fg.Running(chat)
	switch len(ids) {
	case 1:
		ref.Ref, ref.Source, ref.Candidates = ids[0], "exec_window", nil
	case 0:
		ref.Ref, ref.Source, ref.Candidates = "", "none", nil
	default:
		ref.Ref, ref.Source, ref.Candidates = "", "exec_ambiguous", ids
	}
	return store.WithCallRef(ctx, ref)
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	// The channel follows from the endpoint, not from a statement by the agent
	// (Review M5): /artifacts is the CLI channel, MCP comes via /mcp.
	via := "cli"
	ctx := h.callerCtx(r)
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

// UploadToolDescription is the description of the MCP tool upload_artifact (also mcp_upload_artifact in pi).
const UploadToolDescription = "Sends a file to the user: it is stored as an artifact of this chat and appears in the chat at once as a file message, without approval. " +
	"Use it for result files the user wants to keep or download, not for intermediate files. Only regular files inside /workspace, no symbolic links."

// UploadResultText is what the agent reads after an upload (agw-artifact, MCP).
func UploadResultText(r UploadResult) string {
	if r.Status == UploadStored {
		return fmt.Sprintf("sent: %q is in the chat for the user (%d bytes, sha256 %s)", r.Name, r.Size, r.SHA256)
	}
	msg := r.Message
	if msg == "" {
		msg = "not stored"
	}
	return fmt.Sprintf("not sent: %q was not stored (%s)", r.Name, msg)
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
	if res.Status == UploadRejected && res.Message != "" {
		result += ": " + res.Message
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	if err == nil {
		res.Text = UploadResultText(res)
	}
	// name, size and SHA-256 of what was sent; the tool call comes from the context (Activity, evaluation)
	sum := sha256.Sum256(data)
	h.logCall(ctx, chat, via, "upload", fmt.Sprintf("%s (%d bytes, sha256 %s)", name, len(data), hex.EncodeToString(sum[:])), result)
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

// internetRequest is the body of POST /internet.
type internetRequest struct {
	Kind      string   `json:"kind"`
	Reason    string   `json:"reason"`
	Domains   []string `json:"domains"`
	Packages  []string `json:"packages"`
	Minutes   int      `json:"minutes"`
	Megabytes int      `json:"megabytes"`
}

func (r internetRequest) spec() netgrant.Spec {
	return netgrant.Spec{Kind: netgrant.Kind(r.Kind), Reason: truncReason(r.Reason), Domains: r.Domains, Packages: r.Packages, Minutes: r.Minutes, MB: r.Megabytes}
}

// kindError is the answer to a request without a valid kind: the old form (reason only) would otherwise silently
// become full access.
func kindError() string {
	return "kind is required: one of " + netgrant.KindList() + " (search: web_search and web_extract only; domains: https to listed hosts; packages: pip and npm; full: everything)"
}

func (h *handler) internet(w http.ResponseWriter, r *http.Request) {
	via := h.restVia()
	var req internetRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req)
	req.Reason = truncReason(req.Reason)
	if _, ok := netgrant.ParseKind(req.Kind); !ok {
		h.b.LogCall(h.slot, h.b.ChatForSlot(h.slot), via, "internet", req.Reason, "refused: no valid kind")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": kindError()})
		return
	}
	chat, err := h.chat(via, "internet", req.Reason)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	res, err := h.doInternet(h.callerCtx(r), chat, via, req.spec())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) doInternet(ctx context.Context, chat, via string, spec netgrant.Spec) (UploadResult, error) {
	res, err := h.b.RequestInternet(ctx, chat, h.slot, via, spec)
	result := res.Status
	if res.Log != "" {
		result = res.Log
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	h.logCall(ctx, chat, via, "internet", spec.Reason, result)
	return res, err
}

// internetStatus: GET /internet lists the chat's active grants (agw-internet status).
func (h *handler) internetStatus(w http.ResponseWriter, r *http.Request) {
	chat := h.b.ChatForSlot(h.slot)
	if chat == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	grants := []store.Grant{}
	if sb, ok := h.b.(InternetStatusBackend); ok {
		if g := sb.ActiveGrantViews(chat); g != nil {
			grants = g
		}
	}
	out := map[string]any{"grants": grants}
	if rb, ok := h.b.(InternetRefusalsBackend); ok {
		if rs := rb.RecentRefusals(chat); len(rs) > 0 {
			type refusal struct {
				Host    string    `json:"host"`
				Package string    `json:"package,omitempty"`
				Reason  string    `json:"reason,omitempty"`
				Denied  string    `json:"denied,omitempty"`
				At      time.Time `json:"at"`
			}
			list := make([]refusal, 0, len(rs))
			for _, x := range rs {
				list = append(list, refusal{Host: x.Host, Package: x.Package, Reason: x.Reason, Denied: x.Denied, At: x.StartedAt})
			}
			out["blocked"] = list
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// internetOff: POST /internet/off ends one grant ({"grant_id": n}) or all (no body). No approval.
func (h *handler) internetOff(w http.ResponseWriter, r *http.Request) {
	via := h.restVia()
	var req struct {
		GrantID int64 `json:"grant_id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req)
	chat, err := h.chat(via, "internet_off", "")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	res, err := h.doInternetOff(h.callerCtx(r), chat, via, req.GrantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// subagentRelayResult takes the reply of a relay command (issue #81). Nothing is logged: the reply only changes the
// state of a message, and an unknown ref answers 404 without a trace.
func (h *handler) subagentRelayResult(w http.ResponseWriter, r *http.Request) {
	chat := h.b.ChatForSlot(h.slot)
	rb, ok := h.b.(SubagentRelayBackend)
	if chat == "" || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown"})
		return
	}
	var res SubagentRelayResult
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&res); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	res.Message = truncReason(res.Message)
	if err := rb.SubagentRelayResult(h.callerCtx(r), chat, res); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// doInternetOff ends internet grants and logs the call (op internet_off, result "off" or "already off"; detail the
// grant id when one was named).
func (h *handler) doInternetOff(ctx context.Context, chat, via string, grantID int64) (UploadResult, error) {
	res, err := h.b.DisableInternet(ctx, chat, h.slot, via, grantID)
	result := strings.ReplaceAll(res.Status, "_", " ")
	if err != nil {
		result = "error: " + err.Error()
	}
	detail := ""
	if grantID > 0 {
		detail = fmt.Sprintf("grant %d", grantID)
	}
	h.logCall(ctx, chat, via, "internet_off", detail, result)
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
			return fmt.Errorf("files together larger than %d MB per call; every upload creates a new dataset, so choose fewer or smaller files", platform.MaxUploadBytes>>20)
		}
		sum := sha256.Sum256(rr.Data)
		req.Uploads = append(req.Uploads, platform.Upload{Field: f.Field, Name: path.Base(f.Path), Data: rr.Data, SHA256: hex.EncodeToString(sum[:])})
	}
	return nil
}

// workspaceNotebookDir is where a file of a notebook run lands in the sandbox (own code C-E3).
const workspaceNotebookDir = "/workspace/notebook"

// The destination of a fetched file: the run id is one plain segment, the name one file name. The
// gateway builds both, so the path cannot leave the directory the agent is told about.
var (
	fileRunRe  = regexp.MustCompile(`^r[0-9]{14}-[0-9a-f]{4}$`)
	fileNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,120}$`)
)

// saveFile writes a file a virtual tool handed over (platform.Result.File) into the workspace and
// replaces the answer's body with where it lies and what it is (notebook_fetch, own code C-E3).
func (h *handler) saveFile(ctx context.Context, res *platform.Result) error {
	if h.run == nil {
		return errors.New("fetch needs the command line sandbox")
	}
	f := res.File
	if !fileRunRe.MatchString(f.Dir) || !fileNameRe.MatchString(f.Name) {
		return errors.New("the file cannot be saved under this name")
	}
	dir := workspaceNotebookDir + "/" + f.Dir
	mk := execproto.Request{Op: execproto.OpMkdir, Path: dir}
	if err := mk.Validate(); err != nil {
		return err
	}
	if _, err := h.run.Run(ctx, mk, nil); err != nil {
		return err
	}
	w := execproto.Request{Op: execproto.OpWrite, Path: dir + "/" + f.Name, Data: f.Data}
	if err := w.Validate(); err != nil {
		return err
	}
	fr, err := h.run.Run(ctx, w, nil)
	if err != nil {
		return err
	}
	if fr.Error != "" {
		return errors.New(fr.Error)
	}
	sum := sha256.Sum256(f.Data)
	res.Body = fmt.Sprintf("saved as %s/%s (%d bytes, sha256 %s)", dir, f.Name, len(f.Data), hex.EncodeToString(sum[:]))
	res.File = nil
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
	res, err := h.doPlatform(h.callerCtx(r), chat, "cli", tool, raw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// doPlatform builds the call from the tool table, has the backend execute it
// (with confirmation if it writes) and logs it.
func (h *handler) doPlatform(ctx context.Context, chat, via string, tool platform.Tool, raw json.RawMessage) (platform.Result, error) {
	if tool.HasSteps() {
		// The invocation lets an approved plan recognise the tool call (issue #70); a raw request has none.
		ctx = platform.WithInvocation(ctx, tool.Name, raw)
		// The chat goes with the call, so that a step that creates an object can record where it came
		// from (R1.6: the origin of a model); a raw request carries no chat.
		ctx = platform.WithChat(ctx, chat)
		// Every step goes through runPlatform like a single call: delegation, approval and log.
		res, refused, err := tool.Run(ctx, raw, func(ctx context.Context, r platform.Request) (platform.Result, error) {
			n, err := platform.Normalize(r)
			if err != nil {
				h.logCall(ctx, chat, via, "platform", tool.Name, "refused: "+err.Error())
				return platform.Result{Status: "error", Message: err.Error()}, nil
			}
			return h.runPlatform(ctx, chat, via, n)
		})
		if refused != "" {
			h.logCall(ctx, chat, via, "platform", tool.Name, "refused: "+refused)
		}
		return res, err
	}
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
	ctx = h.withExecRef(ctx, chat)
	pb, ok := h.b.(PlatformBackend)
	if !ok {
		res := platform.Result{Status: "error", Message: platform.ErrNotConfigured.Error(), Decision: platform.DecisionError, Reason: "not_configured"}
		h.logPlatform(ctx, chat, via, req.String(), "not configured", res)
		return res, nil
	}
	if len(req.Files) > 0 {
		if pc, ok := h.b.(PlatformPrechecker); ok {
			if res, allowed := pc.PlatformPrecheck(ctx, chat, req); !allowed {
				h.logPlatform(ctx, chat, via, req.String(), "violation blocked: "+res.Message, res)
				return res, nil
			}
		}
		if err := h.readFiles(ctx, &req); err != nil {
			res := platform.Result{Status: "error", Message: err.Error(), Decision: platform.DecisionError, Reason: "upload_failed"}
			h.logPlatform(ctx, chat, via, req.String(), "refused: "+err.Error(), res)
			return res, nil
		}
	}
	if req.Path == platform.PlanPath {
		if err := h.pinPlanFiles(ctx, &req); err != nil {
			res := platform.Result{Status: "error", Message: err.Error(), Decision: platform.DecisionError, Reason: "plan_files_failed"}
			h.logPlatform(ctx, chat, via, req.String(), "refused: "+err.Error(), res)
			return res, nil
		}
	}
	res, err := pb.PlatformCall(ctx, chat, h.slot, via, req)
	if err == nil && res.File != nil {
		// A virtual tool handed a file over instead of an answer (notebook_fetch, own code C-E3): it
		// goes into the workspace, and the agent gets where it lies and what it is.
		if serr := h.saveFile(ctx, &res); serr != nil {
			res.Decision, res.Reason = platform.DecisionError, "save_failed"
			h.logPlatform(ctx, chat, via, req.String(), "error: "+serr.Error(), res)
			return platform.Result{Status: "error", Message: serr.Error()}, nil
		}
	}
	result := res.Status
	if res.HTTPStatus != 0 {
		result = fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	}
	switch {
	case res.Status == "denied":
		result = "violation blocked: " + res.Message
	case res.Status == "error" && strings.HasPrefix(res.Message, platform.RefusedPrefix):
		result = res.Message // refused by the gateway before the platform (issue #63)
	case res.Violation != "":
		result += " · violation, logged only: " + res.Violation
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	if res.Duration > 0 {
		ctx = store.WithDuration(ctx, res.Duration)
	}
	if eff := delegation.Classify(req).Effect; eff.Valid() {
		ctx = store.WithEffect(ctx, string(eff))
	}
	h.logPlatform(ctx, chat, via, req.String(), platform.RedactText(result), res)
	return res, err
}

// PlatformLogger records a platform call with its structured Result (X5, implemented by chat.Manager).
type PlatformLogger interface {
	LogPlatformCall(ctx context.Context, slotID, chatID, via, detail, result string, res platform.Result)
}

// logPlatform logs with the structured record when the backend offers it; the plain log stays the fallback for
// tests and other backends.
func (h *handler) logPlatform(ctx context.Context, chat, via, detail, result string, res platform.Result) {
	if l, ok := h.b.(PlatformLogger); ok {
		l.LogPlatformCall(ctx, h.slot, chat, via, detail, result, res)
		return
	}
	h.logCall(ctx, chat, via, "platform", detail, result)
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
	h.logCall(h.callerCtx(r), chat, "cli", "list", "", fmt.Sprintf("%d entries", len(items)))
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
		h.logCall(h.callerCtx(r), chat, "cli", "get", kind+"/"+name, "not found")
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	defer rc.Close()
	h.logCall(h.callerCtx(r), chat, "cli", "get", kind+"/"+name, "ok")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, rc)
}

// --- MCP ---

type pingIn struct{}
type listIn struct{}
type internetOffIn struct {
	GrantID int64 `json:"grant_id,omitempty" jsonschema:"the grant to switch off; omit to switch all of this chat's internet access off"`
}
type internetIn struct {
	Kind      string   `json:"kind" jsonschema:"search (web_search and web_extract only) or domains (https downloads from the listed host names)"`
	Reason    string   `json:"reason" jsonschema:"short, concrete reason for the user: what is the access needed for?"`
	Domains   []string `json:"domains,omitempty" jsonschema:"kind domains: exact host names, no wildcards, at most 10"`
	Minutes   int      `json:"minutes,omitempty" jsonschema:"proposed duration in minutes; the gateway limits it"`
	Megabytes int      `json:"megabytes,omitempty" jsonschema:"proposed data limit in MB; the gateway limits it"`
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
	mcp.AddTool(s, &mcp.Tool{Name: "upload_artifact", Description: UploadToolDescription},
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
			return textResult(res.Status != UploadStored, "%s", UploadResultText(res)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "request_internet", Description: RequestInternetDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in internetIn) (*mcp.CallToolResult, any, error) {
			in.Reason = truncReason(in.Reason)
			if _, ok := netgrant.ParseKind(in.Kind); !ok {
				h.b.LogCall(h.slot, h.b.ChatForSlot(h.slot), "mcp", "internet", in.Reason, "refused: no valid kind")
				return textResult(true, "refused: %s", kindError()), nil, nil
			}
			chat, err := h.chat("mcp", "internet", in.Reason)
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			res, err := h.doInternet(ctx, chat, "mcp", internetRequest{Kind: in.Kind, Reason: in.Reason, Domains: in.Domains, Minutes: in.Minutes, Megabytes: in.Megabytes}.spec())
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			switch res.Status {
			case "approved":
				return textResult(false, "approved: %s", res.Message), nil, nil
			case "refused":
				return textResult(false, "refused: %s", res.Message), nil, nil
			}
			return textResult(false, "rejected: %s", res.Message), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "disable_internet", Description: "Ends internet access for this chat again (all grants, or the one named by grant_id), e.g. once downloads or web research are done. Needs no approval; if nothing is active, nothing changes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in internetOffIn) (*mcp.CallToolResult, any, error) {
			chat, err := h.chat("mcp", "internet_off", "")
			if err != nil {
				return textResult(true, "error: %v", err), nil, nil
			}
			res, err := h.doInternetOff(ctx, chat, "mcp", in.GrantID)
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
				res, err := h.doPlatform(h.callerExtra(ctx, req), chat, "mcp", t, req.Params.Arguments)
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
	ctx := h.callerCtx(r)
	var res platform.Result
	switch {
	case path == "/_agw/paths" && strings.HasPrefix(q["prefix"], "/_agw"):
		// The arguments of the gateway's own paths, answered inside the gateway (#199).
		body := platform.GatewayIndexDetail(q["prefix"])
		h.logCall(ctx, chat, via, "platform", "api_paths", "ok")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body)
		return
	case path == "/_agw/paths":
		// Condensed path directory like the tool api_paths (the OpenAPI description is ~100 KB).
		tool, _ := platform.Lookup("api_paths")
		args, _ := json.Marshal(map[string]string{"prefix": q["prefix"]})
		res, err = h.doPlatform(ctx, chat, via, tool, args)
	case strings.ToLower(path) == "/_agw" || strings.HasPrefix(strings.ToLower(path), "/_agw/"):
		// A gateway path is the tool of the same name (#199): the call is built like the tool's, so the
		// checks, the delegation and the approval are the same as in the other bindings. A path without a
		// tool, the notebook paths and the reading paths of the gateway are refused here.
		tool, args, why := platform.GatewayCall(r.Method, path, q, bytes.TrimSpace(raw))
		if why != "" {
			h.logCall(ctx, chat, via, "platform", r.Method+" "+path, "refused: "+why)
			fail(http.StatusBadRequest, why)
			return
		}
		if args == nil { // rights, capabilities, plan: passed through
			res, err = h.runGatewayRaw(ctx, chat, via, r.Method, path, q, raw)
			break
		}
		res, err = h.doPlatform(ctx, chat, via, tool, args)
		if err == nil && res.HTTPStatus == 0 && res.Status == "error" && strings.Contains(res.Message, "unknown argument") {
			res.Message += platform.RESTHint
		}
	default:
		res, err = h.runGatewayRaw(ctx, chat, via, r.Method, path, q, raw)
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
	if res.Note != "" { // the gateway's note to the agent, as CLI and MCP get it in Result.Text() (#198)
		w.Header().Set("X-Agw-Note", url.PathEscape(res.Note))
	}
	w.WriteHeader(res.HTTPStatus)
	_, _ = io.WriteString(w, res.Body)
}

// runGatewayRaw passes a REST call on to the platform as it is (after Normalize): the paths of the platform
// itself and the gateway's pass-through paths /_agw/rights, /_agw/capabilities and /_agw/plan.
func (h *handler) runGatewayRaw(ctx context.Context, chat, via, method, path string, q map[string]string, raw []byte) (platform.Result, error) {
	req := platform.Request{Method: method, Path: path, Body: bytes.TrimSpace(raw), Full: true}
	if len(q) > 0 {
		req.Query = q
	}
	if len(req.Body) == 0 {
		req.Body = nil
	}
	norm, nerr := platform.Normalize(req)
	if nerr != nil {
		h.logCall(ctx, chat, via, "platform", method+" "+path, "refused: "+nerr.Error())
		return platform.Result{Status: "error", Message: nerr.Error()}, nil
	}
	return h.runPlatform(ctx, chat, via, norm)
}

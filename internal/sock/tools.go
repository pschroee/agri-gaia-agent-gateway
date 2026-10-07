package sock

// Tool endpoints at pi's socket (E9). The extension exec-bridge.ts
// replaces pi's tools bash, read, write, edit, grep, find and ls
// with versions that send every operation here. The orchestrator executes
// them in the execution sandbox and records each one in tool_executions.
//
//	POST /tool/op      a file operation or search, JSON response
//	POST /tool/bash    command, response as an NDJSON stream; closing the connection aborts
//	POST /tool/upload  mcp_upload_artifact: file from the execution sandbox as an artifact
//	POST /tool/workflow workflowScript of pi-subagents: worker in the execution sandbox,
//	                   NDJSON in both directions on one connection (closing aborts)
//	POST /tool/bg/start  bash with run_in_background: start a background task, immediate response
//	POST /tool/bg/output bg_output: state and tail of the output of a background task
//	POST /tool/bg/stop   bg_stop: end a background task
//
// pi's socket is mounted only in the pi container; the execution
// sandbox has its own socket without these endpoints.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/semaphore"

	"agw/internal/artifacts"
	"agw/internal/bgtask"
	"agw/internal/execproto"
	"agw/internal/store"
)

// ToolRunner executes operations in the execution sandbox (execbox.Client).
type ToolRunner interface {
	Run(ctx context.Context, req execproto.Request, onData func([]byte)) (execproto.Frame, error)
}

// DuplexRunner executes an operation with further input (workflow; execbox.Client).
type DuplexRunner interface {
	RunDuplex(ctx context.Context, req execproto.Request, input <-chan []byte, onData func([]byte)) (execproto.Frame, error)
}

// BackgroundTasks are the background tasks of the slot (bgtask.Registry).
type BackgroundTasks interface {
	Start(ctx context.Context, p bgtask.StartParams) (store.BackgroundTask, error)
	Output(ctx context.Context, chatID, id string) (store.BackgroundTask, string, error)
	Stop(ctx context.Context, chatID, id, by string) (store.BackgroundTask, error)
	Max() int
	// Adopt takes over a running foreground command as a background task.
	Adopt(ctx context.Context, p bgtask.StartParams, logPath string, soFar []byte, cancel context.CancelFunc) (*bgtask.Adopted, error)
}

// ToolRecorder stores an executed operation (manager).
type ToolRecorder interface {
	RecordToolExecution(e store.ToolExecution)
}

// toolOps: which operations a tool may trigger. pi only calls them this way;
// the list keeps the log readable and rules out mixed forms.
var toolOps = map[string][]string{
	"bash":  {execproto.OpBash},
	"read":  {execproto.OpAccess, execproto.OpRead, execproto.OpImageType, execproto.OpStat, execproto.OpReadLines},
	"write": {execproto.OpMkdir, execproto.OpWrite},
	"edit":  {execproto.OpAccess, execproto.OpRead, execproto.OpWrite},
	"ls":    {execproto.OpStat, execproto.OpReaddir},
	"grep":  {execproto.OpGrep, execproto.OpStat},
	"find":  {execproto.OpGlob, execproto.OpStat},
}

// UploadTool is the MCP tool whose file is in the execution sandbox.
const UploadTool = "mcp_upload_artifact"

// Tools of the background tasks (bash with run_in_background starts them).
const (
	BgOutputTool = "bg_output"
	BgStopTool   = "bg_stop"
)

// ExecutedTools are the tools whose execution is recorded at the socket.
func ExecutedTools() []string {
	out := []string{UploadTool, BgOutputTool, BgStopTool}
	for k := range toolOps {
		out = append(out, k)
	}
	return out
}

var sessionRe = regexp.MustCompile(`^/agent/sessions/[^/]+/([0-9a-fA-F-]{8,64})/run-(\d+)/session\.jsonl$`)

// SessionKey turns the path of the session file into the ID for the
// log: "main" for the main session, otherwise the ID of the
// subagent run (with #n for parallel children), as in subagent_entries.
func SessionKey(file string) string {
	m := sessionRe.FindStringSubmatch(file)
	if m == nil {
		return "main"
	}
	if m[2] == "0" {
		return m[1]
	}
	return m[1] + "#" + m[2]
}

func validToolCallID(id string) bool {
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

type toolRequest struct {
	ToolCallID  string            `json:"toolCallId"`
	Tool        string            `json:"tool"`
	SessionFile string            `json:"sessionFile"`
	Req         execproto.Request `json:"req"`
	// only /tool/upload
	Path string `json:"path"`
	Name string `json:"name"`
}

type toolHandler struct {
	*handler
	run ToolRunner
	rec ToolRecorder
	// Limits per slot (N4): at most maxToolRequests requests at once, and operations with
	// large content (read, write, upload) take from a byte budget. A read can return up to
	// MaxFileBytes, which sit in the orchestrator's memory several times over as raw data, JSON
	// and response; without a limit, 64 parallel reads took several GB.
	slots chan struct{}
	bytes *semaphore.Weighted
	bg    BackgroundTasks // nil: no background tasks
	fg    *Foreground     // running foreground commands (stop, conversion by the user)
}

const (
	maxToolRequests = 32
	// toolByteBudget: two reads at the limit (MaxFileBytes) at once.
	toolByteBudget = 2 * execproto.MaxFileBytes
)

// acquire holds a request until the slot's limits allow it; weight is the
// expected content in bytes (0: only the count matters).
func (th *toolHandler) acquire(ctx context.Context, weight int64) (func(), error) {
	select {
	case th.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	weight = min(weight, toolByteBudget)
	if weight > 0 {
		if err := th.bytes.Acquire(ctx, weight); err != nil {
			<-th.slots
			return nil, err
		}
	}
	return func() {
		if weight > 0 {
			th.bytes.Release(weight)
		}
		<-th.slots
	}, nil
}

// NewPiHandler serves pi's socket: MCP and the tool endpoints (without background tasks).
func NewPiHandler(slotID string, b Backend, maxBytes int64, run ToolRunner, rec ToolRecorder) http.Handler {
	return NewPiHandlerBg(slotID, b, maxBytes, run, rec, nil)
}

// NewPiHandlerBg is NewPiHandler with the slot's background tasks.
func NewPiHandlerBg(slotID string, b Backend, maxBytes int64, run ToolRunner, rec ToolRecorder, bg BackgroundTasks) http.Handler {
	return NewPiHandlerFg(slotID, b, maxBytes, run, rec, bg, NewForeground())
}

// NewPiHandlerFg is NewPiHandlerBg with the register of foreground commands, through which the user
// stops a running command or converts it into a background task.
func NewPiHandlerFg(slotID string, b Backend, maxBytes int64, run ToolRunner, rec ToolRecorder, bg BackgroundTasks, fg *Foreground) http.Handler {
	h := newHandler(slotID, b, maxBytes)
	h.run = run      // platform uploads via MCP read from the same execution sandbox
	h.apiVia = "api" // REST endpoint at pi's socket: the HTTP tool of the variant api
	th := &toolHandler{handler: h, run: run, rec: rec, slots: make(chan struct{}, maxToolRequests), bytes: semaphore.NewWeighted(toolByteBudget), bg: bg, fg: fg}
	mux := http.NewServeMux()
	mux.Handle("/mcp", h.mcp)
	mux.HandleFunc("POST /tool/op", th.op)
	mux.HandleFunc("POST /tool/bash", th.bash)
	mux.HandleFunc("POST /tool/upload", th.upload)
	mux.HandleFunc("POST /tool/workflow", th.workflow)
	mux.HandleFunc("POST /tool/bg/start", th.bgStart)
	mux.HandleFunc("POST /tool/bg/output", th.bgOutput)
	mux.HandleFunc("POST /tool/bg/stop", th.bgStop)
	mux.HandleFunc("GET /tool/internet", th.internetState)
	return h.withPlatformAPI(mux)
}

// decode reads and checks the request; on an error the response has already been written. The
// slot's limits (N4) apply before reading the body; release frees them again and must
// be called on an error too.
func (th *toolHandler) decode(w http.ResponseWriter, r *http.Request, bash bool) (tr toolRequest, chat string, release func(), ok bool) {
	release = func() {}
	rel, err := th.acquire(r.Context(), max(r.ContentLength, 0))
	if err != nil {
		return tr, "", release, false
	}
	release = rel
	tr, chat, ok = th.decodeBody(w, r, bash)
	if !ok {
		return tr, chat, release, false
	}
	// A read (also for the upload) can return up to Max: reserve that in addition.
	extra := int64(0)
	switch {
	case r.URL.Path == "/tool/upload":
		extra = th.max
	case tr.Req.Op == execproto.OpRead:
		extra = tr.Req.Max
	}
	if extra > 0 {
		extra = min(extra, toolByteBudget)
		if err := th.bytes.Acquire(r.Context(), extra); err != nil {
			return tr, chat, release, false
		}
		release = func() { th.bytes.Release(extra); rel() }
	}
	return tr, chat, release, true
}

func (th *toolHandler) decodeBody(w http.ResponseWriter, r *http.Request, bash bool) (toolRequest, string, bool) {
	var tr toolRequest
	limit := int64(execproto.MaxFileBytes)*4/3 + 1<<20
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	if err := dec.Decode(&tr); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable request"})
		return tr, "", false
	}
	chat, err := th.chat("tool", "tool", tr.Tool)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "slot not assigned"})
		return tr, "", false
	}
	if !validToolCallID(tr.ToolCallID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid toolCallId"})
		return tr, "", false
	}
	if r.URL.Path == "/tool/upload" {
		if tr.Tool != UploadTool {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload only for " + UploadTool})
			return tr, "", false
		}
		return tr, chat, true
	}
	ops, ok := toolOps[tr.Tool]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tool not redirected: " + tr.Tool})
		return tr, "", false
	}
	if bash != (tr.Req.Op == execproto.OpBash) || !contains(ops, tr.Req.Op) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("operation %q does not belong to %s", tr.Req.Op, tr.Tool)})
		return tr, "", false
	}
	if err := tr.Req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return tr, "", false
	}
	return tr, chat, true
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// digest collects checksum, size and an excerpt (start and end) of the output.
type digest struct {
	h     hash.Hash
	n     int64
	head  []byte
	tail  []byte
	limit int
}

func newDigest(limit int) *digest { return &digest{h: sha256.New(), limit: limit} }

func (d *digest) Write(p []byte) {
	d.h.Write(p)
	d.n += int64(len(p))
	half := d.limit / 2
	if len(d.head) < half {
		k := min(half-len(d.head), len(p))
		d.head = append(d.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		d.tail = append(d.tail, p...)
		if len(d.tail) > half {
			d.tail = d.tail[len(d.tail)-half:]
		}
	}
}

func (d *digest) Excerpt() string {
	s := string(d.head)
	if d.n > int64(len(d.head)+len(d.tail)) {
		s += fmt.Sprintf("\n… [%d bytes omitted] …\n", d.n-int64(len(d.head)+len(d.tail)))
	}
	s += string(d.tail)
	return noNUL(s)
}

// noNUL: Postgres accepts no NUL in text and no \u0000 in jsonb (K1). The excerpt
// replaces it with U+2400 (␀); checksum and size apply to the real bytes.
func noNUL(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "\u2400"), "\uFFFD")
}

func (d *digest) Sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

const excerptBytes = 4096

// summarize shortens the arguments for the log; file contents appear only as
// size and checksum.
func summarize(req execproto.Request) json.RawMessage {
	m := map[string]any{}
	switch req.Op {
	case execproto.OpBash, execproto.OpBg:
		c := req.Command
		if len(c) > 4000 {
			c = strings.ToValidUTF8(c[:4000], "") + " … [truncated]"
		}
		m["command"], m["cwd"] = c, req.Cwd
		if req.Timeout > 0 {
			m["timeout"] = req.Timeout
		}
	case execproto.OpWrite:
		sum := sha256.Sum256(req.Data)
		m["path"], m["bytes"], m["sha256"] = req.Path, len(req.Data), hex.EncodeToString(sum[:])
	case execproto.OpGrep:
		m["path"], m["grep"] = req.Path, req.Grep
	case execproto.OpGlob:
		m["path"], m["glob"] = req.Path, req.Glob
	default:
		m["path"] = req.Path
		if req.Mode != "" {
			m["mode"] = req.Mode
		}
	}
	for k, v := range m {
		if str, ok := v.(string); ok {
			m[k] = noNUL(str)
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func (th *toolHandler) record(chat string, tr toolRequest, op string, args json.RawMessage, f execproto.Frame, err error, d *digest, start time.Time) {
	e := store.ToolExecution{ChatID: chat, SlotID: th.slot, Session: SessionKey(tr.SessionFile), ToolCallID: tr.ToolCallID,
		Tool: tr.Tool, Op: op, Args: args, ExitCode: f.Exit, Error: f.Error, OutputExcerpt: d.Excerpt(), OutputSHA256: d.Sum(),
		OutputBytes: d.n, StartedAt: start, DurationMs: time.Since(start).Milliseconds()}
	if err != nil && e.Error == "" {
		e.Error = err.Error()
	}
	if f.Code != "" && f.Code != e.Error {
		e.Error = strings.TrimSpace(f.Code + ": " + e.Error)
	}
	e.Error = noNUL(e.Error)
	th.rec.RecordToolExecution(e)
}

func (th *toolHandler) op(w http.ResponseWriter, r *http.Request) {
	tr, chat, release, ok := th.decode(w, r, false)
	defer release()
	if !ok {
		return
	}
	args := summarize(tr.Req)
	start := time.Now()
	f, err := th.run.Run(r.Context(), tr.Req, nil)
	d := newDigest(excerptBytes)
	if tr.Req.Op == execproto.OpRead && f.Error == "" {
		var rr execproto.ReadResult
		if json.Unmarshal(f.Result, &rr) == nil {
			d.Write(rr.Data)
		}
	} else {
		d.Write(f.Result)
	}
	th.record(chat, tr, tr.Req.Op, args, f, err, d, start)
	if err != nil && f.Error == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	f.ID = 0
	writeJSON(w, http.StatusOK, f)
}

// bash streams the output as NDJSON: {"data":"<base64>"} per chunk and at the
// end {"done":true,"exit":n} or {"done":true,"error":…,"code":…}.
//
// The command runs in its own context so that the user can stop it (code "stopped") or convert
// it into a background task (code "backgrounded", ID in background): then the tool call ends
// immediately, the command keeps running, and its output goes to the task. The command's
// timeout is therefore enforced by the orchestrator, not agw-exec; after a conversion it no longer
// applies. If pi closes the connection (abort), the command ends as before.
func (th *toolHandler) bash(w http.ResponseWriter, r *http.Request) {
	tr, chat, release, ok := th.decode(w, r, true)
	defer release()
	if !ok {
		return
	}
	// The full output ends up in a file whose path the orchestrator derives from the toolCallId
	// (H1); the bridge tells the model the path, as pi does.
	tr.Req.Spill = execproto.SpillPath(tr.ToolCallID)
	args := summarize(tr.Req)
	start := time.Now()
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if fl != nil {
		fl.Flush()
	}
	enc := json.NewEncoder(w)
	d := newDigest(excerptBytes)

	opCtx, opCancel := context.WithCancel(context.Background())
	op := th.fg.add(tr.ToolCallID, chat, opCancel)
	finished := make(chan struct{})
	defer close(op.done)
	// Abort by pi (connection closed), as long as the command has not been converted.
	var detached atomic.Bool
	go func() {
		select {
		case <-r.Context().Done():
			if !detached.Load() {
				opCancel()
			}
		case <-finished:
		}
	}()
	var timedOut atomic.Bool
	var timer *time.Timer
	if tr.Req.Timeout > 0 {
		timer = time.AfterFunc(time.Duration(tr.Req.Timeout*float64(time.Second)), func() {
			timedOut.Store(true)
			opCancel()
		})
	}
	req := tr.Req
	req.Timeout = 0
	req.Env = withToolCallEnv(req.Env, tr.ToolCallID, SessionKey(tr.SessionFile))

	// Until the conversion the output goes to pi; after that to the background task. soFar keeps the
	// tail of the output so far for the task.
	var mu sync.Mutex
	var adopted *bgtask.Adopted
	soFar := bgtask.NewTail(bgtask.TailBytes)
	onData := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		if adopted != nil {
			adopted.Write(b)
			return
		}
		d.Write(b)
		soFar.Write(b)
		_ = enc.Encode(map[string]string{"data": base64.StdEncoding.EncodeToString(b)})
		if fl != nil {
			fl.Flush()
		}
	}
	type result struct {
		f   execproto.Frame
		err error
	}
	res := make(chan result, 1)
	go func() {
		f, err := th.run.Run(opCtx, req, onData)
		res <- result{f, err}
	}()

	for {
		select {
		case rr := <-res:
			close(finished)
			opCancel()
			if timer != nil {
				timer.Stop()
			}
			th.fg.remove(tr.ToolCallID, op)
			f, err := rr.f, rr.err
			switch {
			case op.stopped.Load():
				f = execproto.Frame{Done: true, Error: "Command stopped by the user", Code: "stopped", FullOutputPath: f.FullOutputPath, SpillError: f.SpillError}
				err = nil
			case timedOut.Load():
				f = execproto.Frame{Done: true, Error: "timeout", Code: "timeout", FullOutputPath: f.FullOutputPath, SpillError: f.SpillError}
				err = nil
			}
			th.record(chat, tr, execproto.OpBash, args, f, err, d, start)
			if err != nil && f.Error == "" {
				f = execproto.Frame{Done: true, Error: err.Error(), Code: "EIO"}
				if errors.Is(err, context.Canceled) {
					f.Error, f.Code = "aborted", "aborted"
				}
			}
			mu.Lock()
			f.ID, f.Done = 0, true
			_ = enc.Encode(f)
			mu.Unlock()
			return
		case dr := <-op.detach:
			if th.bg == nil {
				dr.reply <- detachResult{err: ErrNoBackground}
				continue
			}
			mu.Lock()
			a, err := th.bg.Adopt(dr.ctx, bgtask.StartParams{ChatID: chat, Session: SessionKey(tr.SessionFile), ToolCallID: tr.ToolCallID,
				Command: tr.Req.Command, Cwd: tr.Req.Cwd}, tr.Req.Spill, soFar.Bytes(), opCancel)
			if err != nil {
				mu.Unlock()
				dr.reply <- detachResult{err: err}
				continue
			}
			adopted = a
			detached.Store(true)
			if timer != nil {
				timer.Stop()
			}
			th.fg.remove(tr.ToolCallID, op)
			// The foreground part has been executed; the rest is in background_tasks.
			th.record(chat, tr, execproto.OpBash, args, execproto.Frame{Error: "moved to background as " + a.Task.ID, Code: "backgrounded"}, nil, d, start)
			_ = enc.Encode(execproto.Frame{Done: true, Code: "backgrounded", Background: a.Task.ID, FullOutputPath: tr.Req.Spill})
			mu.Unlock()
			go func() {
				rr := <-res
				close(finished)
				opCancel()
				a.Finish(rr.f, rr.err)
			}()
			dr.reply <- detachResult{task: a.Task}
			return
		}
	}
}

// ToolCallEnv: environment variable through which agw-artifact learns the ID of the tool call (for
// showing the artifact in the history). The orchestrator sets it itself after the check.
const ToolCallEnv = "PI_AGW_TOOL_CALL_ID"

// SessionEnv: session of the call ("main" or subagent run, like SessionKey), for mapping
// approvals and socket calls in the UI.
const SessionEnv = "PI_AGW_SESSION"

func withToolCallEnv(env map[string]string, id, session string) map[string]string {
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	out[ToolCallEnv] = id
	out[SessionEnv] = session
	return out
}

// InternetStater reports a chat's internet switch (manager). The extension web-gate.ts in pi
// uses it to show and hide web_search and web_extract.
type InternetStater interface {
	InternetOn(ctx context.Context, chatID string) bool
}

// internetState: GET /tool/internet → {"enabled": bool}. The switch is enforced at the web proxy;
// this only controls whether the tools are offered to the model.
//
// A freshly started slot asks once before it is assigned (web-gate.ts on session_start). That is
// expected and answered quietly, without a log entry: a slot without a chat has no internet, so the
// answer is the same 409 as before, and the extension treats it as "off".
func (th *toolHandler) internetState(w http.ResponseWriter, r *http.Request) {
	chat := th.b.ChatForSlot(th.slot)
	if chat == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "slot not assigned"})
		return
	}
	on := false
	if s, ok := th.b.(InternetStater); ok {
		on = s.InternetOn(r.Context(), chat)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": on})
}

// upload: mcp_upload_artifact names a path; the file is in the
// execution sandbox. The orchestrator reads it there (logged like a
// read) and passes it on to the upload with confirmation.
func (th *toolHandler) upload(w http.ResponseWriter, r *http.Request) {
	tr, chat, release, ok := th.decode(w, r, false)
	defer release()
	if !ok {
		return
	}
	req := execproto.Request{Op: execproto.OpRead, Path: tr.Path, Max: th.max}
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	args := summarize(req)
	start := time.Now()
	f, err := th.run.Run(r.Context(), req, nil)
	d := newDigest(excerptBytes)
	var rr execproto.ReadResult
	if err == nil && f.Error == "" {
		_ = json.Unmarshal(f.Result, &rr)
		d.Write(rr.Data)
	}
	th.record(chat, tr, execproto.OpRead, args, f, err, d, start)
	if err != nil || f.Error != "" {
		msg := f.Error
		if msg == "" {
			msg = err.Error()
		}
		if f.Code == "EFBIG" {
			th.b.LogCall(th.slot, chat, "mcp", "upload", tr.Path, "refused: too large")
			msg = fmt.Sprintf("file larger than %d MB", th.max>>20)
		}
		writeJSON(w, http.StatusOK, map[string]string{"error": msg})
		return
	}
	name := artifacts.SanitizeName(tr.Name)
	if name == "" {
		name = artifacts.SanitizeName(tr.Path[strings.LastIndexByte(tr.Path, '/')+1:])
	}
	if name == "" {
		writeJSON(w, http.StatusOK, map[string]string{"error": "invalid name"})
		return
	}
	res, err := th.doUpload(store.WithSession(store.WithToolCall(r.Context(), tr.ToolCallID), SessionKey(tr.SessionFile)), chat, "mcp", name, rr.Data, "")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// WorkflowTool: the script of a workflow (subagent with workflowScript) runs under this
// tool; the execution is logged like any other.
const WorkflowTool = "subagent"

// workflow: the bridge (remote-worker.mjs) replaces the worker thread in which pi-subagents runs
// the script of a workflow. First line of the request: toolCallId, session and the
// worker's source code (from pi-subagents, not from the agent); after that one host message
// {"m": …} per line. The response carries, line by line, what the worker writes in the execution
// sandbox, and at the end {"done":true,"exit":n}. Both directions run at the same time.
func (th *toolHandler) workflow(w http.ResponseWriter, r *http.Request) {
	dr, ok := th.run.(DuplexRunner)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "workflow not supported"})
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	br := bufio.NewReaderSize(r.Body, 64<<10)
	first, err := readLine(br, execproto.MaxWorkflowSource*2)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable request"})
		return
	}
	var tr struct {
		ToolCallID  string `json:"toolCallId"`
		Tool        string `json:"tool"`
		SessionFile string `json:"sessionFile"`
		Source      string `json:"source"`
	}
	if json.Unmarshal(first, &tr) != nil || tr.Tool != WorkflowTool {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workflow only for " + WorkflowTool})
		return
	}
	chat, err := th.chat("tool", "workflow", tr.Tool)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "slot not assigned"})
		return
	}
	if !validToolCallID(tr.ToolCallID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid toolCallId"})
		return
	}
	req := execproto.Request{Op: execproto.OpWorkflow, Data: []byte(tr.Source)}
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	release, err := th.acquire(r.Context(), 0)
	if err != nil {
		return
	}
	defer release()
	ctx := r.Context()
	// Messages from the host; the first one ("start") carries the agent's script for the log.
	var script string
	scriptSeen := make(chan struct{})
	input := make(chan []byte, 64)
	go func() {
		defer close(input)
		seen := false
		for {
			line, err := readLine(br, 16<<20)
			if err != nil {
				if !seen {
					close(scriptSeen)
				}
				return
			}
			if !seen {
				var m struct {
					M struct {
						Type   string `json:"type"`
						Script string `json:"script"`
					} `json:"m"`
				}
				if json.Unmarshal(line, &m) == nil && m.M.Type == "start" {
					script, seen = m.M.Script, true
					close(scriptSeen)
				}
			}
			select {
			case input <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	start := time.Now()
	d := newDigest(excerptBytes)
	f, err := dr.RunDuplex(ctx, req, input, func(b []byte) {
		d.Write(b)
		d.Write([]byte("\n"))
		_, _ = w.Write(append(append([]byte(nil), b...), '\n'))
		_ = rc.Flush()
	})
	select {
	case <-scriptSeen:
	case <-time.After(time.Second):
	}
	sum := sha256.Sum256([]byte(script))
	sc := script
	if len(sc) > 4000 {
		sc = strings.ToValidUTF8(sc[:4000], "") + " … [truncated]"
	}
	args, _ := json.Marshal(map[string]any{"workflowScript": noNUL(sc), "bytes": len(script), "sha256": hex.EncodeToString(sum[:])})
	th.record(chat, toolRequest{ToolCallID: tr.ToolCallID, Tool: tr.Tool, SessionFile: tr.SessionFile}, execproto.OpWorkflow, args, f, err, d, start)
	if err != nil && f.Error == "" {
		f = execproto.Frame{Done: true, Error: err.Error(), Code: "EIO"}
		if errors.Is(err, context.Canceled) {
			f.Error, f.Code = "aborted", "aborted"
		}
	}
	f.ID, f.Done = 0, true
	b, _ := json.Marshal(f)
	_, _ = w.Write(append(b, '\n'))
	_ = rc.Flush()
}

// readLine reads a line (without "\n") with an upper limit.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, io.ErrShortBuffer
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

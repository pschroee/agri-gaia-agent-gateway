package sock

// Werkzeug-Endpunkte am Socket von pi (E9). Die Extension exec-bridge.ts
// ersetzt in pi die Werkzeuge bash, read, write, edit, grep, find und ls
// durch Fassungen, die jede Operation hierher schicken. Der Orchestrator führt
// sie in der Ausführungs-Sandbox aus und trägt jede in tool_executions ein.
//
//	POST /tool/op      eine Dateioperation oder Suche, Antwort JSON
//	POST /tool/bash    Befehl, Antwort als NDJSON-Strom; Schließen der Verbindung bricht ab
//	POST /tool/upload  mcp_upload_artifact: Datei aus der Ausführungs-Sandbox als Artefakt
//	POST /tool/workflow workflowScript von pi-subagents: Worker in der Ausführungs-Sandbox,
//	                   NDJSON in beide Richtungen auf einer Verbindung (Schließen bricht ab)
//	POST /tool/bg/start  bash mit run_in_background: Hintergrundaufgabe starten, Antwort sofort
//	POST /tool/bg/output bg_output: Stand und Ende der Ausgabe einer Hintergrundaufgabe
//	POST /tool/bg/stop   bg_stop: Hintergrundaufgabe beenden
//
// Der Socket von pi ist nur im Container von pi eingehängt; die
// Ausführungs-Sandbox hat einen eigenen Socket ohne diese Endpunkte.

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

// ToolRunner führt Operationen in der Ausführungs-Sandbox aus (execbox.Client).
type ToolRunner interface {
	Run(ctx context.Context, req execproto.Request, onData func([]byte)) (execproto.Frame, error)
}

// DuplexRunner führt eine Operation mit weiteren Eingaben aus (workflow; execbox.Client).
type DuplexRunner interface {
	RunDuplex(ctx context.Context, req execproto.Request, input <-chan []byte, onData func([]byte)) (execproto.Frame, error)
}

// BackgroundTasks sind die Hintergrundaufgaben des Platzes (bgtask.Registry).
type BackgroundTasks interface {
	Start(ctx context.Context, p bgtask.StartParams) (store.BackgroundTask, error)
	Output(ctx context.Context, chatID, id string) (store.BackgroundTask, string, error)
	Stop(ctx context.Context, chatID, id, by string) (store.BackgroundTask, error)
	Max() int
	// Adopt übernimmt einen laufenden Vordergrundbefehl als Hintergrundaufgabe.
	Adopt(ctx context.Context, p bgtask.StartParams, logPath string, soFar []byte, cancel context.CancelFunc) (*bgtask.Adopted, error)
}

// ToolRecorder speichert eine ausgeführte Operation (Manager).
type ToolRecorder interface {
	RecordToolExecution(e store.ToolExecution)
}

// toolOps: welche Operationen ein Werkzeug auslösen darf. pi ruft sie nur so
// auf; die Liste hält das Protokoll lesbar und schließt Mischformen aus.
var toolOps = map[string][]string{
	"bash":  {execproto.OpBash},
	"read":  {execproto.OpAccess, execproto.OpRead, execproto.OpImageType, execproto.OpStat, execproto.OpReadLines},
	"write": {execproto.OpMkdir, execproto.OpWrite},
	"edit":  {execproto.OpAccess, execproto.OpRead, execproto.OpWrite},
	"ls":    {execproto.OpStat, execproto.OpReaddir},
	"grep":  {execproto.OpGrep, execproto.OpStat},
	"find":  {execproto.OpGlob, execproto.OpStat},
}

// UploadTool ist das MCP-Werkzeug, dessen Datei in der Ausführungs-Sandbox liegt.
const UploadTool = "mcp_upload_artifact"

// Werkzeuge der Hintergrundaufgaben (bash mit run_in_background startet sie).
const (
	BgOutputTool = "bg_output"
	BgStopTool   = "bg_stop"
)

// ExecutedTools sind die Werkzeuge, deren Ausführung am Socket belegt wird.
func ExecutedTools() []string {
	out := []string{UploadTool, BgOutputTool, BgStopTool}
	for k := range toolOps {
		out = append(out, k)
	}
	return out
}

var sessionRe = regexp.MustCompile(`^/agent/sessions/[^/]+/([0-9a-fA-F-]{8,64})/run-(\d+)/session\.jsonl$`)

// SessionKey macht aus dem Pfad der Sitzungsdatei die Kennung für das
// Protokoll: "main" für die Hauptsitzung, sonst die Kennung des
// Subagenten-Laufs (bei parallelen Kindern mit #n), wie in subagent_entries.
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
	// nur /tool/upload
	Path string `json:"path"`
	Name string `json:"name"`
}

type toolHandler struct {
	*handler
	run ToolRunner
	rec ToolRecorder
	// Grenzen je Platz (N4): höchstens maxToolRequests Anfragen zugleich, und Operationen mit
	// großem Inhalt (read, write, Upload) belegen ein Byte-Budget. Ein read kann bis zu
	// MaxFileBytes liefern, die im Orchestrator als Rohdaten, JSON und Antwort mehrfach im
	// Speicher stehen; ohne Grenze belegten 64 parallele reads mehrere GB.
	slots chan struct{}
	bytes *semaphore.Weighted
	bg    BackgroundTasks // nil: keine Hintergrundaufgaben
	fg    *Foreground     // laufende Vordergrundbefehle (Stopp, Umwandlung durch den Nutzer)
}

const (
	maxToolRequests = 32
	// toolByteBudget: zwei reads an der Grenze (MaxFileBytes) zugleich.
	toolByteBudget = 2 * execproto.MaxFileBytes
)

// acquire hält eine Anfrage an, bis die Grenzen des Platzes sie zulassen; weight ist der
// erwartete Inhalt in Bytes (0: nur die Anzahl zählt).
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

// NewPiHandler bedient den Socket von pi: MCP und die Werkzeug-Endpunkte (ohne Hintergrundaufgaben).
func NewPiHandler(slotID string, b Backend, maxBytes int64, run ToolRunner, rec ToolRecorder) http.Handler {
	return NewPiHandlerBg(slotID, b, maxBytes, run, rec, nil)
}

// NewPiHandlerBg ist NewPiHandler mit den Hintergrundaufgaben des Platzes.
func NewPiHandlerBg(slotID string, b Backend, maxBytes int64, run ToolRunner, rec ToolRecorder, bg BackgroundTasks) http.Handler {
	return NewPiHandlerFg(slotID, b, maxBytes, run, rec, bg, NewForeground())
}

// NewPiHandlerFg ist NewPiHandlerBg mit dem Register der Vordergrundbefehle, über das der Nutzer
// einen laufenden Befehl stoppt oder in eine Hintergrundaufgabe umwandelt.
func NewPiHandlerFg(slotID string, b Backend, maxBytes int64, run ToolRunner, rec ToolRecorder, bg BackgroundTasks, fg *Foreground) http.Handler {
	h := newHandler(slotID, b, maxBytes)
	h.run = run      // Plattform-Uploads über MCP lesen aus derselben Ausführungs-Sandbox
	h.apiVia = "api" // REST-Endpunkt am Socket von pi: das HTTP-Werkzeug der Variante api
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

// decode liest und prüft die Anfrage; bei einem Fehler ist die Antwort schon geschrieben. Vor
// dem Lesen des Rumpfs gelten die Grenzen des Platzes (N4); release gibt sie wieder frei und
// ist auch bei einem Fehler aufzurufen.
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
	// Ein read (auch für den Upload) kann bis zu Max liefern: zusätzlich belegen.
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

// digest sammelt Prüfsumme, Größe und einen Auszug (Anfang und Ende) der Ausgabe.
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
		s += fmt.Sprintf("\n… [%d Bytes ausgelassen] …\n", d.n-int64(len(d.head)+len(d.tail)))
	}
	s += string(d.tail)
	return noNUL(s)
}

// noNUL: Postgres nimmt kein NUL in text und kein \u0000 in jsonb an (K1). Der Auszug
// ersetzt es durch U+2400 (␀); Prüfsumme und Größe gelten für die echten Bytes.
func noNUL(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "\u2400"), "\uFFFD")
}

func (d *digest) Sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

const excerptBytes = 4096

// summarize kürzt die Argumente für das Protokoll; Dateiinhalte stehen nur als
// Größe und Prüfsumme darin.
func summarize(req execproto.Request) json.RawMessage {
	m := map[string]any{}
	switch req.Op {
	case execproto.OpBash, execproto.OpBg:
		c := req.Command
		if len(c) > 4000 {
			c = strings.ToValidUTF8(c[:4000], "") + " … [gekürzt]"
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

// bash streamt die Ausgabe als NDJSON: {"data":"<base64>"} je Stück und zum
// Schluss {"done":true,"exit":n} bzw. {"done":true,"error":…,"code":…}.
//
// Der Befehl läuft in einem eigenen Kontext, damit der Nutzer ihn stoppen (Code "stopped") oder in
// eine Hintergrundaufgabe umwandeln kann (Code "backgrounded", Kennung in background): Dann endet
// der Werkzeugaufruf sofort, der Befehl läuft weiter, und seine Ausgabe geht an die Aufgabe. Die
// Zeitgrenze des Befehls hält deshalb der Orchestrator, nicht agw-exec; nach einer Umwandlung gilt
// sie nicht mehr. Schließt pi die Verbindung (Abbruch), endet der Befehl wie bisher.
func (th *toolHandler) bash(w http.ResponseWriter, r *http.Request) {
	tr, chat, release, ok := th.decode(w, r, true)
	defer release()
	if !ok {
		return
	}
	// Die ganze Ausgabe landet in einer Datei, deren Pfad der Orchestrator aus der toolCallId
	// bildet (H1); die Bridge nennt ihn dem Modell wie pi.
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
	// Abbruch durch pi (Verbindung zu), solange der Befehl nicht umgewandelt ist.
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

	// Bis zur Umwandlung geht die Ausgabe an pi; danach an die Hintergrundaufgabe. soFar hält das
	// Ende der bisherigen Ausgabe für die Aufgabe.
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
			// Der Vordergrundteil ist ausgeführt; der Rest steht in background_tasks.
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

// ToolCallEnv: Umgebungsvariable, über die agw-artifact die Kennung des Werkzeugaufrufs erfährt (für die
// Anzeige des Artefakts im Verlauf). Der Orchestrator setzt sie nach der Prüfung selbst.
const ToolCallEnv = "PI_AGW_TOOL_CALL_ID"

// SessionEnv: Sitzung des Aufrufs („main“ oder Subagenten-Lauf, wie SessionKey), für die Zuordnung von
// Bestätigungen und Socket-Aufrufen in der UI.
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

// InternetStater meldet den Internet-Schalter eines Chats (Manager). Die Extension web-gate.ts in pi
// blendet damit web_search und web_extract ein und aus.
type InternetStater interface {
	InternetOn(ctx context.Context, chatID string) bool
}

// internetState: GET /tool/internet → {"enabled": bool}. Durchgesetzt wird der Schalter am Web-Proxy;
// das hier steuert nur, ob die Werkzeuge dem Modell angeboten werden.
func (th *toolHandler) internetState(w http.ResponseWriter, r *http.Request) {
	chat, err := th.chat("tool", "tool", "internet")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "slot not assigned"})
		return
	}
	on := false
	if s, ok := th.b.(InternetStater); ok {
		on = s.InternetOn(r.Context(), chat)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": on})
}

// upload: mcp_upload_artifact nennt einen Pfad; die Datei liegt in der
// Ausführungs-Sandbox. Der Orchestrator liest sie dort (protokolliert wie ein
// read) und reicht sie an den Upload mit Bestätigung weiter.
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
			th.b.LogCall(th.slot, chat, "mcp", "upload", tr.Path, "abgewiesen: zu groß")
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

// WorkflowTool: Unter diesem Werkzeug läuft das Skript eines Workflows (subagent mit
// workflowScript); die Ausführung wird wie jede andere protokolliert.
const WorkflowTool = "subagent"

// workflow: Die Bridge (remote-worker.mjs) ersetzt den Worker-Thread, in dem pi-subagents das
// Skript eines Workflows ausführt. Erste Zeile der Anfrage: toolCallId, Sitzung und der
// Quelltext des Workers (von pi-subagents, nicht vom Agenten); danach je Zeile eine Nachricht
// des Hosts {"m": …}. Die Antwort trägt je Zeile, was der Worker in der Ausführungs-Sandbox
// schreibt, und zum Schluss {"done":true,"exit":n}. Beide Richtungen laufen gleichzeitig.
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
	// Nachrichten des Hosts; die erste („start“) trägt das Skript des Agenten fürs Protokoll.
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
		sc = strings.ToValidUTF8(sc[:4000], "") + " … [gekürzt]"
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

// readLine liest eine Zeile (ohne „\n“) mit Obergrenze.
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

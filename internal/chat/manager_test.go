package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/config"
	"agw/internal/llmproxy"
	"agw/internal/pool"
	"agw/internal/rpc"
	"agw/internal/store"
	"agw/internal/titler"
)

// fakeAgent spielt pi: prompt erzeugt eine Antwort samt agent_settled.
type fakeAgent struct {
	id       string
	mu       sync.Mutex
	cmds     []map[string]any
	files    map[string][]byte
	internet bool
	closed   bool
	events   chan rpc.Event
	pollOut  string   // Antwort auf das Einlese-Skript der Subagenten
	execs    []string // ausgeführte Kommandos
	reply    string   // Text der Antwort (sonst „Antwort von …“)

	wsOut        []byte // Ausgabe des Sicherungsskripts für /workspace (sonst „unverändert“)
	wsSaves      int    // Aufrufe des Sicherungsskripts
	wsRestored   []byte // tar-Strom, der zum Einspielen kam
	wsLatePrompt bool   // beim Einspielen war schon ein prompt gekommen

	execDone chan struct{} // Ausführungs-Sandbox beendet (H2)

	hold       chan struct{} // gesetzt: der Lauf endet erst, wenn der Kanal geschlossen ist
	failSwitch bool          // switch_session meldet cancelled (Fortsetzen scheitert)

	// onPrompt läuft während des Durchgangs (nach agent_start und der Nutzernachricht, vor der
	// Antwort); promptWait: So lange antwortet der Aufruf prompt nicht (Zeitlimit nach Annahme).
	onPrompt   func(msg string)
	promptWait time.Duration
	streaming  bool   // zwischen prompt und agent_settled (get_state: isStreaming)
	onSwitch   func() // läuft beim Einspielen der Sitzung (Fortsetzen)

	ctxTokens int64 // gemeldeter Kontext (0: 4200); compact setzt ihn auf 2000

	withTool bool     // der Lauf ruft ein Werkzeug auf (tool_execution_start vor hold, _end danach)
	steered  []string // eingeschleuste Aufträge (prompt mit streamingBehavior steer), noch nicht eingefügt
	thinking string   // Denkstufe (get_state); set_thinking_level setzt sie
	model    string   // zuletzt per set_model gesetzt
}

func newFakeAgent(id string) *fakeAgent {
	return &fakeAgent{id: id, files: map[string][]byte{}, events: make(chan rpc.Event, 64), execDone: make(chan struct{})}
}

func (a *fakeAgent) ExecDone() <-chan struct{} { return a.execDone }

// crashPi beendet pis Ereignisstrom, ohne dass der Platz abgebaut wird (Absturz von pi).
func (a *fakeAgent) crashPi() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.events)
	}
}

func (a *fakeAgent) emit(raw string) {
	var h struct{ Type string }
	_ = json.Unmarshal([]byte(raw), &h)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.events <- rpc.Event{Type: h.Type, Raw: json.RawMessage(raw)}
}

func (a *fakeAgent) Call(_ context.Context, cmd map[string]any) (rpc.Response, error) {
	a.mu.Lock()
	a.cmds = append(a.cmds, cmd)
	a.mu.Unlock()
	switch cmd["type"] {
	case "get_state":
		a.mu.Lock()
		// pi hat die Sitzung geschrieben, sobald eine Antwort kam.
		if _, ok := a.files["/agent/sessions/s.jsonl"]; !ok {
			a.files["/agent/sessions/s.jsonl"] = []byte(`{"type":"session","agent":"` + a.id + `"}` + "\n")
		}
		streaming := a.streaming
		a.mu.Unlock()
		thinking := a.thinking
		if thinking == "" {
			thinking = "high"
		}
		return rpc.Response{Success: true, Data: json.RawMessage(fmt.Sprintf(`{"sessionFile":"/agent/sessions/s.jsonl","isStreaming":%v,"thinkingLevel":%q}`, streaming, thinking))}, nil
	case "set_model":
		a.mu.Lock()
		a.model = fmt.Sprint(cmd["provider"], "/", cmd["modelId"])
		a.mu.Unlock()
	case "set_thinking_level":
		a.mu.Lock()
		a.thinking = fmt.Sprint(cmd["level"])
		a.mu.Unlock()
	case "get_available_thinking_levels":
		return rpc.Response{Success: true, Data: json.RawMessage(`{"levels":["off","low","high","max"]}`)}, nil
	case "get_session_stats":
		a.mu.Lock()
		tokens := a.ctxTokens
		a.mu.Unlock()
		if tokens == 0 {
			tokens = 4200
		}
		return rpc.Response{Success: true, Data: json.RawMessage(fmt.Sprintf(`{"contextUsage":{"tokens":%d,"contextWindow":1000000,"percent":0.42}}`, tokens))}, nil
	case "get_commands":
		return rpc.Response{Success: true, Data: json.RawMessage(`{"commands":[{"name":"skill:artifacts","description":"Artefakte","source":"skill"}]}`)}, nil
	case "compact":
		a.mu.Lock()
		if a.ctxTokens != 0 {
			a.ctxTokens = 2000
		}
		a.mu.Unlock()
		go func() {
			a.emit(`{"type":"compaction_start","reason":"manual"}`)
			a.emit(`{"type":"compaction_end","reason":"manual","result":{"summary":"Zusammenfassung","tokensBefore":9000,"estimatedTokensAfter":2000,"usage":{"input":9000,"output":300,"cacheRead":0,"totalTokens":9300,"cost":{"total":0.003}}},"aborted":false,"willRetry":false}`)
		}()
		return rpc.Response{Success: true, Data: json.RawMessage(`{"summary":"Zusammenfassung"}`)}, nil
	case "switch_session":
		a.mu.Lock()
		fail, hook := a.failSwitch, a.onSwitch
		a.mu.Unlock()
		if hook != nil {
			hook()
		}
		if fail {
			return rpc.Response{Success: true, Data: json.RawMessage(`{"cancelled":true}`)}, nil
		}
	case "clear_queue":
		// wie pi: liefert und entfernt, was eingeschleust, aber noch nicht eingefügt ist (abort
		// dagegen setzt Eingereihtes fort)
		a.mu.Lock()
		b, _ := json.Marshal(map[string]any{"steering": append([]string{}, a.steered...), "followUp": []string{}})
		a.steered = nil
		a.mu.Unlock()
		return rpc.Response{Success: true, Data: b}, nil
	case "prompt":
		msg := cmd["message"].(string)
		a.mu.Lock()
		if cmd["streamingBehavior"] == "steer" && a.streaming {
			a.steered = append(a.steered, msg)
			a.mu.Unlock()
			return rpc.Response{Success: true}, nil
		}
		hold := a.hold
		answer := fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Antwort von %s"}],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.001}}}}`, a.id)
		if a.reply != "" {
			answer = fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","responseId":"resp-%s","content":[{"type":"text","text":%q}],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.001}}}}`, a.id, a.reply)
		}
		onPrompt, wait := a.onPrompt, a.promptWait
		a.promptWait = 0 // nur einmal
		a.streaming = true
		a.mu.Unlock()
		go func() {
			a.emit(`{"type":"agent_start"}`)
			a.emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, msg))
			if onPrompt != nil {
				onPrompt(msg)
			}
			a.mu.Lock()
			tool := a.withTool
			a.mu.Unlock()
			if tool {
				a.emit(`{"type":"tool_execution_start","toolCallId":"t1","toolName":"bash"}`)
			}
			if hold != nil {
				<-hold
			}
			if tool {
				a.emit(`{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash"}`)
				// wie pi: Eingeschleustes kommt nach den Werkzeugen, vor dem nächsten Modellaufruf
				a.mu.Lock()
				steered := a.steered
				a.steered = nil
				a.mu.Unlock()
				for _, s := range steered {
					a.emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, s))
				}
			}
			a.emit(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"An"}}`)
			a.emit(answer)
			a.mu.Lock()
			a.streaming = false
			a.mu.Unlock()
			a.emit(`{"type":"agent_settled"}`)
		}()
		if wait > 0 {
			time.Sleep(wait)
			return rpc.Response{}, context.DeadlineExceeded
		}
	}
	return rpc.Response{Success: true, Data: json.RawMessage(`{"cancelled":false}`)}, nil
}

func (a *fakeAgent) Events() <-chan rpc.Event { return a.events }
func (a *fakeAgent) ContainerID() string      { return "c-" + a.id }
func (a *fakeAgent) ContainerName() string    { return "agwpoc-" + a.id }
func (a *fakeAgent) Image() string            { return "test" }

// ExecPi spielt den Container von pi: nur agw-exec und cat, keine Shell.
func (a *fakeAgent) ExecPi(_ context.Context, cmd []string, stdin io.Reader) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.execs = append(a.execs, "pi: "+strings.Join(cmd, " "))
	var b []byte
	if stdin != nil {
		b, _ = io.ReadAll(stdin)
	}
	switch {
	case cmd[0] == "cat":
		return a.files[cmd[1]], nil
	case len(cmd) == 3 && cmd[0] == "agw-exec" && cmd[1] == "put":
		a.files[cmd[2]] = b
		return nil, nil
	case len(cmd) == 2 && cmd[0] == "agw-exec" && cmd[1] == "poll-subagents":
		return []byte(a.pollOut), nil
	case len(cmd) == 2 && cmd[0] == "agw-exec" && cmd[1] == "kill-node":
		return []byte("0\n"), nil
	}
	return nil, errors.New("im Container von pi nicht ausführbar (keine Shell): " + strings.Join(cmd, " "))
}

func (a *fakeAgent) Exec(_ context.Context, cmd []string, stdin io.Reader) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.execs = append(a.execs, strings.Join(cmd, " "))
	script := strings.Join(cmd, " ")
	var b []byte
	if stdin != nil {
		b, _ = io.ReadAll(stdin)
	}
	switch {
	case strings.Contains(script, "realpath -e"): // Bild lesen: sh -c <skript> sh <pfad> <grenze>
		d, ok := a.files[cmd[4]]
		if !ok {
			return nil, errors.New("exec: Exit-Code 3")
		}
		var n int
		fmt.Sscan(cmd[5], &n)
		return d[:min(n, len(d))], nil
	case strings.Contains(script, "agw:workspace-save"):
		a.wsSaves++
		if a.wsOut == nil {
			return []byte("SAME - 0 0\n"), nil
		}
		return a.wsOut, nil
	case strings.Contains(script, "agw:workspace-restore"):
		a.wsRestored = b
		for _, c := range a.cmds {
			if c["type"] == "prompt" {
				a.wsLatePrompt = true
			}
		}
	case strings.Contains(script, "mkdir -p"):
		a.files[cmd[len(cmd)-1]] = b
	default:
		return nil, errors.New("unbekanntes exec: " + script)
	}
	return nil, nil
}

func (a *fakeAgent) IP() string { return "10.0.0." + strings.TrimPrefix(a.id, "a") }
func (a *fakeAgent) Notify(cmd map[string]any) error {
	a.mu.Lock()
	a.cmds = append(a.cmds, cmd)
	a.mu.Unlock()
	return nil
}
func (a *fakeAgent) SetInternet(_ context.Context, on bool) error {
	a.mu.Lock()
	a.internet = on
	a.mu.Unlock()
	return nil
}

func (a *fakeAgent) hasCmds() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cmds) > 0
}

func (a *fakeAgent) commands() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.cmds {
		out = append(out, c["type"].(string))
	}
	return out
}

type memBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (b *memBlobs) Put(_ context.Context, k string, r io.Reader, _ int64, _ string) error {
	d, _ := io.ReadAll(r)
	b.mu.Lock()
	b.m[k] = d
	b.mu.Unlock()
	return nil
}
func (b *memBlobs) Get(_ context.Context, k string) (io.ReadCloser, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.m[k]
	if !ok {
		return nil, 0, store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), int64(len(d)), nil
}
func (b *memBlobs) Move(ctx context.Context, s, d string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[d] = b.m[s]
	delete(b.m, s)
	return nil
}
func (b *memBlobs) Delete(_ context.Context, k string) error {
	b.mu.Lock()
	delete(b.m, k)
	b.mu.Unlock()
	return nil
}
func (b *memBlobs) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.m {
		out = append(out, k)
	}
	return out
}

type env struct {
	m      *Manager
	p      *pool.Pool[Agent]
	cat    *config.Catalog
	opt    Options
	st     *store.Store
	blobs  *memBlobs
	mu     sync.Mutex
	agents []*fakeAgent

	failSwitch bool   // neue Sandboxen lassen switch_session scheitern
	onSwitch   func() // für neue Sandboxen: läuft beim Einspielen der Sitzung
}

func (e *env) agent(i int) *fakeAgent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.agents[i]
}

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL nicht gesetzt")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "chattest_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	e := &env{st: st, blobs: &memBlobs{m: map[string][]byte{}}}
	n := 0
	create := func(ctx context.Context, slotID, variant string) (Agent, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		n++
		a := newFakeAgent(fmt.Sprintf("a%d", n))
		a.failSwitch = e.failSwitch
		a.onSwitch = e.onSwitch
		e.agents = append(e.agents, a)
		return a, nil
	}
	destroy := func(_ context.Context, a Agent) {
		fa := a.(*fakeAgent)
		fa.mu.Lock()
		defer fa.mu.Unlock()
		if !fa.closed {
			fa.closed = true
			close(fa.events) // wie ein abgebauter Container: pis Strom endet
		}
	}
	p := pool.New[Agent](create, destroy, map[string]int{"cli": 1, "mcp": 0, "beide": 0})
	pctx, cancel := context.WithCancel(ctx)
	p.Start(pctx)
	t.Cleanup(func() { cancel(); p.Shutdown(context.Background()) })
	cat, _ := config.ParseCatalog([]byte(`{"default":"deepseek/deepseek-flash","providers":[{"id":"deepseek","models":[{"id":"deepseek-flash"},{"id":"klein","context_window":8000}]}]}`))
	e.p, e.cat = p, cat
	e.opt = Options{ApprovalTimeout: 2 * time.Second, ArtifactMaxBytes: 1 << 20, AcquireTimeout: 3 * time.Second, AutoCompactDefault: true, CompactReserveTokens: 16384, CompactKeepRecent: 20000, MaxSubagentsDefault: 2, MaxSubagentsLimit: 5}
	e.m = NewManager(st, p, cat, e.blobs, artifacts.NewBroker(), e.opt)
	return e
}

func waitEvent(t *testing.T, ch <-chan Event, kind, piType string) Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind != kind {
				continue
			}
			if piType == "" {
				return ev
			}
			var h struct{ Type string }
			_ = json.Unmarshal(ev.Data.(json.RawMessage), &h)
			if h.Type == piType {
				return ev
			}
		case <-timeout:
			t.Fatalf("Ereignis %s/%s kam nicht", kind, piType)
		}
	}
}

func waitSettled(t *testing.T, e *env, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := e.m.View(context.Background(), id)
		msgs, _ := e.st.Messages(context.Background(), id)
		if !v.Running && len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Lauf wurde nicht fertig")
}

func TestCreateSendStoresMessagesAndSession(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	on := true
	c, err := e.m.Create(ctx, NewChat{Title: "t", Internet: &on})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != store.StateActive || c.SlotID == "" || c.Model != "deepseek/deepseek-flash" {
		t.Fatalf("angelegt: %+v", c)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if _, err := e.m.Send(ctx, c.ID, "Hallo"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "pi", "message_update")
	waitEvent(t, events, "pi", "agent_settled")
	waitSettled(t, e, c.ID)

	a := e.agent(0)
	if !a.internet {
		t.Fatal("Internet nicht gesetzt")
	}
	cmds := strings.Join(a.commands(), ",")
	if i, j := strings.Index(cmds, "set_model"), strings.Index(cmds, "prompt"); i != 0 || j < i {
		t.Fatalf("Befehle: %s", cmds)
	}
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("Nachrichten: %+v", msgs)
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.Tokens.Total != 15 || v.Cost < 0.0009 {
		t.Fatalf("Kosten/Tokens: %+v %v", v.Tokens, v.Cost)
	}
	// Die Sitzung wird nach agent_settled im Hintergrund gesichert (Review H1).
	var sess []byte
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if sess, _ = e.st.LoadSession(ctx, c.ID); strings.Contains(string(sess), `"agent":"a1"`) {
			break
		}
	}
	if !strings.Contains(string(sess), `"agent":"a1"`) {
		t.Fatalf("Sitzung nicht gesichert: %q", sess)
	}
}

func TestSuspendAndResumeInFreshSandbox(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Merke dir 42"})
	waitSettled(t, e, c.ID)
	v, err := e.m.Suspend(ctx, c.ID)
	if err != nil || v.State != store.StateDormant || v.SlotID != "" {
		t.Fatalf("Suspend: %+v %v", v, err)
	}
	resumed, err := e.m.Send(ctx, c.ID, "Was war die Zahl?")
	if err != nil || !resumed.Resumed {
		t.Fatalf("Fortsetzen: %v %v", resumed, err)
	}
	waitSettled(t, e, c.ID)
	// Der neue Agent ist nicht der alte (Einmalvergabe) und hat die Sitzung bekommen.
	var fresh *fakeAgent
	e.mu.Lock()
	for _, a := range e.agents {
		a.mu.Lock()
		for _, cmd := range a.cmds {
			if cmd["type"] == "switch_session" {
				fresh = a
			}
		}
		a.mu.Unlock()
	}
	e.mu.Unlock()
	if fresh == nil || fresh.id == "a1" {
		t.Fatal("keine frische Sandbox mit switch_session")
	}
	p := "/agent/sessions/" + c.ID + ".jsonl"
	if got := string(fresh.files[p]); !strings.Contains(got, `"agent":"a1"`) {
		t.Fatalf("Sitzung nicht eingespielt: %q", got)
	}
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 4 {
		t.Fatalf("Nachrichten nach Fortsetzen: %d", len(msgs))
	}
}

func TestUploadNeedsApproval(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()

	type result struct {
		r   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := e.m.Upload(store.WithToolCall(ctx, "call_up"), c.ID, slot, "cli", "zahlen.csv", 4, "", strings.NewReader("1,2\n"))
		done <- result{r.Status, err}
	}()
	ev := waitEvent(t, events, "approval", "")
	ap := ev.Data.(store.Approval)
	if ap.State != "pending" || ap.Preview != "1,2\n" {
		t.Fatalf("Anfrage: %+v", ap)
	}
	info, _ := e.m.pool.Get(slot)
	if info.Info().Activity.Kind != "waiting_approval" {
		t.Fatalf("Tätigkeit: %+v", info.Info().Activity)
	}
	if _, err := e.m.Suspend(ctx, c.ID); !errors.Is(err, ErrPendingApproval) {
		t.Fatalf("Ruhen trotz offener Bestätigung: %v", err)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.r != "approved" {
		t.Fatalf("Upload: %+v", r)
	}
	arts, _ := e.st.ListArtifacts(ctx, c.ID)
	if len(arts) != 1 || arts[0].Kind != "output" || arts[0].ToolCallID != "call_up" {
		t.Fatalf("Artefakte: %+v", arts)
	}
	for _, k := range e.blobs.keys() {
		if strings.HasPrefix(k, "pending/") {
			t.Fatalf("ausstehendes Objekt blieb liegen: %s", k)
		}
	}
}

func TestUploadRejected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		r, _ := e.m.Upload(ctx, c.ID, slot, "mcp", "x.bin", 3, "", bytes.NewReader([]byte{0, 1, 2}))
		done <- r.Status
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if s := <-done; s != "rejected" {
		t.Fatalf("Ablehnung kommt nicht an: %s", s)
	}
	if len(e.blobs.keys()) != 0 {
		t.Fatalf("Objekte übrig: %v", e.blobs.keys())
	}
}

func TestUploadTimeoutExpires(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 50 * time.Millisecond
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	r, err := e.m.Upload(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", "a.txt", 1, "", strings.NewReader("a"))
	if err != nil || r.Status != "rejected" || !strings.Contains(r.Message, "Wartezeit") {
		t.Fatalf("Ablauf: %+v %v", r, err)
	}
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if len(aps) != 1 || aps[0].State != "expired" {
		t.Fatalf("Zustand: %+v", aps)
	}
}

func TestInternetToggleAndInputs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	off := false
	c, _ := e.m.Create(ctx, NewChat{Internet: &off})
	a := e.agent(0)
	if a.internet {
		t.Fatal("Internet an, obwohl aus gewünscht")
	}
	if _, err := e.m.SetInternet(ctx, c.ID, true); err != nil || !a.internet {
		t.Fatalf("Umschalten: %v %v", err, a.internet)
	}
	if _, err := e.m.AddInput(ctx, c.ID, "../daten.csv", []byte("x,y\n")); err != nil {
		t.Fatal(err)
	}
	if string(a.files["/workspace/inputs/daten.csv"]) != "x,y\n" {
		t.Fatalf("Eingabe nicht gespiegelt: %v", a.files)
	}
	// Nach dem Fortsetzen liegt die Eingabe auch in der neuen Sandbox.
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, c.ID, "weiter"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	var fresh *fakeAgent
	e.mu.Lock()
	fresh = e.agents[len(e.agents)-1]
	for _, ag := range e.agents {
		if ag != a && ag.hasCmds() {
			fresh = ag
		}
	}
	e.mu.Unlock()
	if string(fresh.files["/workspace/inputs/daten.csv"]) != "x,y\n" || !fresh.internet {
		t.Fatalf("frische Sandbox: Dateien %v, Internet %v", fresh.files, fresh.internet)
	}
}

func TestUnknownModelAndVariant(t *testing.T) {
	e := setup(t)
	if _, err := e.m.Create(context.Background(), NewChat{Model: "x/y"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("Modell: %v", err)
	}
	if _, err := e.m.Create(context.Background(), NewChat{Variant: "shell"}); !errors.Is(err, ErrUnknownVariant) {
		t.Fatalf("Variante: %v", err)
	}
}

func TestBillUsesTariffAtResponseTime(t *testing.T) {
	cat, err := config.ParseCatalog([]byte(`{"default":"deepseek/deepseek-flash","providers":[{"id":"deepseek",
	  "tariff":{"peak_windows_utc":[{"days":"mon-fri","from":"06:00","to":"10:00"}],"offpeak_factor":0.5},
	  "models":[{"id":"deepseek-flash","pricing":{"input":0.3,"output":1.2,"cache_read":0.006}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cat: cat}
	// Dienstag 07:00 UTC (Spitze) und 12:00 UTC (Nebenzeit), je 1 Mio. Ausgabe-Tokens.
	peakMsg := json.RawMessage(`{"role":"assistant","provider":"deepseek","model":"deepseek-flash","timestamp":1790665200000,"usage":{"output":1000000}}`)
	offMsg := json.RawMessage(`{"role":"assistant","provider":"deepseek","model":"deepseek-flash","timestamp":1790683200000,"usage":{"output":1000000}}`)
	b1, b2 := m.bill(context.Background(), "", peakMsg), m.bill(context.Background(), "", offMsg)
	if b1 == nil || !b1.Peak || b1.Cost != 1.2 {
		t.Fatalf("Spitze: %+v", b1)
	}
	if b2 == nil || b2.Peak || b2.Cost != 0.6 {
		t.Fatalf("Nebenzeit: %+v", b2)
	}
}

func TestContextAfterSettle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "hallo"})
	waitSettled(t, e, c.ID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := e.m.View(ctx, c.ID)
		if v.Context != nil {
			var u ContextUsage
			_ = json.Unmarshal(v.Context, &u)
			if u.Tokens == nil || *u.Tokens != 4200 || u.Window != 1000000 || u.ThresholdTokens != int64(1000000-e.m.opt.CompactReserveTokens) {
				t.Fatalf("Kontext: %+v", u)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Kontext nicht gespeichert")
}

func TestCompactCommandStoresCompaction(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "hallo"})
	waitSettled(t, e, c.ID)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if _, err := e.m.RunCommand(ctx, c.ID, "/compact Fokus auf Zahlen"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "pi", "compaction_end")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := e.m.View(ctx, c.ID)
		if v.Compactions == 1 {
			msgs, _ := e.st.Messages(ctx, c.ID)
			last := msgs[len(msgs)-1]
			if last.Role != "compaction" || !strings.Contains(string(last.Message), "Zusammenfassung") {
				t.Fatalf("Eintrag: %+v", last)
			}
			var found bool
			for _, cmd := range e.agent(0).cmds {
				if cmd["type"] == "compact" && cmd["customInstructions"] == compactLanguageHint+" Fokus auf Zahlen" {
					found = true
				}
			}
			if !found {
				t.Fatal("compact ohne Anweisungen gesendet")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Kompaktierung nicht gespeichert")
}

func TestAutoCompactToggleAndResume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.RunCommand(ctx, c.ID, "/autocompact off"); err != nil {
		t.Fatal(err)
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.AutoCompact {
		t.Fatal("Automatik noch an")
	}
	sent := func(a *fakeAgent) (bool, bool) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var last, seen bool
		for _, cmd := range a.cmds {
			if cmd["type"] == "set_auto_compaction" {
				seen, last = true, cmd["enabled"].(bool)
			}
		}
		return seen, last
	}
	if seen, last := sent(e.agent(0)); !seen || last {
		t.Fatalf("set_auto_compaction nicht aus: %v %v", seen, last)
	}
	_, _ = e.m.Suspend(ctx, c.ID)
	_, _ = e.m.Send(ctx, c.ID, "weiter")
	waitSettled(t, e, c.ID)
	e.mu.Lock()
	fresh := e.agents[len(e.agents)-1]
	for _, ag := range e.agents[1:] {
		if ag.hasCmds() {
			fresh = ag
		}
	}
	e.mu.Unlock()
	if seen, last := sent(fresh); !seen || last {
		t.Fatalf("frische Sandbox bekam die Einstellung nicht: %v %v", seen, last)
	}
}

func TestCommandsListAndPassThrough(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	cmds, err := e.m.Commands(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, cm := range cmds {
		names[cm.Name] = cm.Source
	}
	if names["compact"] != "builtin" || names["autocompact"] != "builtin" || names["skill:artifacts"] != "skill" {
		t.Fatalf("Befehle: %v", names)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/skill:artifacts list"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	if !strings.Contains(string(msgs[0].Message), "/skill:artifacts list") {
		t.Fatalf("nicht als Prompt weitergereicht: %s", msgs[0].Message)
	}
	// Nach dem Ruhen bleibt die Liste bekannt.
	_, _ = e.m.Suspend(ctx, c.ID)
	cmds, _ = e.m.Commands(ctx, c.ID)
	if len(cmds) < 3 {
		t.Fatalf("Liste nach Ruhen: %+v", cmds)
	}
}

func TestInternetRequest(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	off := false
	c, _ := e.m.Create(ctx, NewChat{Internet: &off})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", "Ich muss eine Python-Bibliothek installieren.")
		done <- r.Status
	}()
	ev := waitEvent(t, events, "approval", "")
	ap := ev.Data.(store.Approval)
	if ap.Kind != "internet_access" || !strings.Contains(ap.Name, "Bibliothek") {
		t.Fatalf("Anfrage: %+v", ap)
	}
	if e.agent(0).internet {
		t.Fatal("Internet vor der Entscheidung an")
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if s := <-done; s != "approved" {
		t.Fatalf("Status: %s", s)
	}
	v, _ := e.m.View(ctx, c.ID)
	if !e.agent(0).internet || !v.Internet {
		t.Fatal("Internet nach Zustimmung nicht an")
	}
	// Schon an: sofort bestätigt, ohne neue Anfrage.
	r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", "nochmal")
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if r.Status != "approved" || len(aps) != 1 {
		t.Fatalf("zweite Anfrage: %+v, %d Bestätigungen", r, len(aps))
	}
}

func TestInternetRequestRejected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	off := false
	c, _ := e.m.Create(ctx, NewChat{Internet: &off})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		r, _ := e.m.RequestInternet(ctx, c.ID, slot, "mcp", "Abruf einer Webseite")
		done <- r.Status
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	_, _ = e.m.Decide(ctx, ap.ID, false)
	if s := <-done; s != "rejected" {
		t.Fatalf("Status: %s", s)
	}
	if e.agent(0).internet {
		t.Fatal("Internet trotz Ablehnung an")
	}
}

// Nach dem Ruhen endet pis Strom, weil der Container abgebaut wird.
// Das darf nicht als Absturz der Sandbox gelten und detach nicht doppelt auslösen
// (früher: "close of closed channel" und Absturz des Orchestrators).
func TestDetachThenStreamEndDoesNotPanic(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		c, err := e.m.Create(ctx, NewChat{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = e.m.Suspend(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	list, _ := e.m.List(ctx)
	for _, c := range list {
		if c.State == store.StateActive {
			t.Fatalf("Chat nach dem Ruhen wieder aktiv: %+v", c)
		}
	}
}

// M7: Entscheidet der Nutzer genau dann, wenn die Wartezeit abläuft, gilt die
// Entscheidung aus der Datenbank.
func TestDecisionWinsOverExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	ap, _ := e.st.CreateApproval(ctx, store.Approval{ChatID: c.ID, Kind: "artifact_upload", Via: "cli", Name: "x", ContentType: "text/plain", PendingKey: "p"})
	if _, _, err := e.st.DecideApproval(ctx, ap.ID, store.ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	if got := e.m.settle(ctx, c.ID, ap.ID, false, artifacts.ErrTimeout); got != store.ApprovalApproved {
		t.Fatalf("Zustand: %s", got)
	}
}

// H3: Internet umschalten wartet, bis ein laufendes Fortsetzen fertig ist, und
// wirkt dann auf die neue Sandbox.
func TestSetInternetDuringResume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	on := true
	c, _ := e.m.Create(ctx, NewChat{Internet: &on, Message: "hallo"})
	waitSettled(t, e, c.ID)
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = e.m.Send(ctx, c.ID, "weiter"); close(done) }()
	time.Sleep(5 * time.Millisecond)
	if _, err := e.m.SetInternet(ctx, c.ID, false); err != nil {
		t.Fatal(err)
	}
	<-done
	waitSettled(t, e, c.ID)
	e.mu.Lock()
	last := e.agents[len(e.agents)-1]
	for _, a := range e.agents {
		if a.hasCmds() {
			last = a
		}
	}
	e.mu.Unlock()
	v, _ := e.m.View(ctx, c.ID)
	last.mu.Lock()
	defer last.mu.Unlock()
	if v.Internet || last.internet {
		t.Fatalf("Internet: DB %v, Sandbox %v – beide müssen aus sein", v.Internet, last.internet)
	}
}

func TestAttributeAndRecord(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	three := 3
	c, _ := e.m.Create(ctx, NewChat{MaxSubagents: &three})
	a := e.agent(0)
	att := e.m.Attribute(a.IP())
	if att.ChatID != c.ID || att.MaxConcurrent != 4 {
		t.Fatalf("Zuordnung: %+v", att)
	}
	if got := e.m.Attribute("10.9.9.9"); got.ChatID != "" {
		t.Fatalf("fremde Adresse zugeordnet: %+v", got)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	e.m.Record(llmproxy.Call{ChatID: c.ID, SlotID: att.SlotID, SourceIP: a.IP(), Model: "deepseek/deepseek-flash", ResponseID: "r1", Status: 200,
		Usage: config.Usage{Input: 10, Output: 5}, Cost: 0.002, StartedAt: time.Now(), ToolCalls: []llmproxy.ToolCall{{Name: "bash", Arguments: "{}"}}})
	waitEvent(t, events, "llm_call", "")
	v, _ := e.m.View(ctx, c.ID)
	if v.LLMCalls != 1 || v.Cost != 0.002 || v.CostOther != 0.002 {
		t.Fatalf("nach Aufruf: %+v", v.Chat)
	}
	// Nach dem Ruhen gehört die Adresse zu keinem Chat mehr.
	_, _ = e.m.Suspend(ctx, c.ID)
	if got := e.m.Attribute(a.IP()); got.ChatID != "" {
		t.Fatalf("Adresse nach Ruhen noch zugeordnet: %+v", got)
	}
}

func TestMaxSubagentsBoundsAndConfig(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	if v, _ := e.m.View(ctx, c.ID); v.MaxSubagents != 2 {
		t.Fatalf("Vorgabe: %d", v.MaxSubagents)
	}
	if _, err := e.m.SetMaxSubagents(ctx, c.ID, 6); !errors.Is(err, ErrInvalid) {
		t.Fatalf("über der Obergrenze: %v", err)
	}
	if _, err := e.m.SetMaxSubagents(ctx, c.ID, 1); err != nil {
		t.Fatal(err)
	}
	if att := e.m.Attribute(e.agent(0).IP()); att.MaxConcurrent != 2 {
		t.Fatalf("Grenze wirkt nicht sofort am Proxy: %+v", att)
	}
	a := e.agent(0)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.Contains(strings.Join(a.execs, "\n"), "extensions/subagent/config.json") {
		t.Fatalf("pi-subagents-Konfiguration nicht geschrieben: %v", a.execs)
	}
}

func pollJSON(runs ...string) string {
	type file struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Data   string `json:"data"`
	}
	var fs []file
	agents := map[string]string{}
	for i, r := range runs {
		d := `{"type":"message","id":"u` + r + `","message":{"role":"user","content":[{"type":"text","text":"Task ` + r + `"}]}}` + "\n" +
			`{"type":"message","id":"m` + r + `","message":{"role":"assistant","responseId":"resp-` + r + `","content":[{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}]}}` + "\n"
		fs = append(fs, file{Path: "/agent/sessions/s/" + r + "/run-0/session.jsonl", Offset: int64(len(d) + i), Data: d})
		agents[r] = "scout"
	}
	b, _ := json.Marshal(map[string]any{"files": fs, "agents": agents,
		"runs": map[string]any{runs[0]: map[string]any{"agent": "scout", "label": "suche", "state": "running"}}})
	return string(b)
}

func TestSubagentEntriesAndHardLimit(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	one := 1
	c, _ := e.m.Create(ctx, NewChat{MaxSubagents: &one})
	a := e.agent(0)
	e.m.mu.Lock()
	l := e.m.live[c.ID]
	e.m.mu.Unlock()
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()

	run1 := "11111111-1111-1111-1111-111111111111"
	run2 := "22222222-2222-2222-2222-222222222222"
	a.mu.Lock()
	a.pollOut = pollJSON(run1)
	a.mu.Unlock()
	offsets := map[string]int64{}
	e.m.pollSubagents(c.ID, l, offsets, map[string]runInfo{}, map[string]string{})
	if r := waitEvent(t, events, "subagent_run", "").Data.(store.SubagentRun); r.RunID != run1 || r.Label != "suche" || r.State != "running" {
		t.Fatalf("Lauf: %+v", r)
	}
	ev := waitEvent(t, events, "subagent", "")
	if se := ev.Data.(store.SubagentEntry); se.RunID != run1 || se.Agent != "scout" {
		t.Fatalf("Eintrag: %+v", se)
	}
	a.mu.Lock()
	aborted := strings.Contains(fmt.Sprint(a.cmds), "abort")
	a.mu.Unlock()
	if aborted {
		t.Fatal("innerhalb der Grenze abgebrochen")
	}
	// Zweiter Lauf überschreitet die Grenze von 1: Abbruch und Prozesse beenden.
	a.mu.Lock()
	a.pollOut = pollJSON(run1, run2)
	a.mu.Unlock()
	e.m.pollSubagents(c.ID, l, map[string]int64{}, map[string]runInfo{}, map[string]string{})
	waitEvent(t, events, "error", "")
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.Contains(fmt.Sprint(a.cmds), "abort") {
		t.Fatal("kein Abbruch bei überschrittener Grenze")
	}
	if !strings.Contains(strings.Join(a.execs, "\n"), "pi: agw-exec kill-node") {
		t.Fatal("Subagenten-Prozesse nicht beendet")
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.Subagents != 2 {
		t.Fatalf("gezählte Läufe: %d", v.Subagents)
	}
}

func TestExtensionUIIsCancelled(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	a := e.agent(0)
	a.emit(`{"type":"extension_ui_request","id":"ui-1","method":"confirm","title":"Mehr Subagenten erlauben?"}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for _, cmd := range a.cmds {
			if cmd["type"] == "extension_ui_response" && cmd["id"] == "ui-1" && cmd["cancelled"] == true {
				a.mu.Unlock()
				calls, _ := e.m.SocketCalls(ctx, c.ID)
				if len(calls) == 0 || calls[len(calls)-1].Op != "extension_ui" {
					t.Fatalf("nicht protokolliert: %+v", calls)
				}
				return
			}
		}
		a.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Rückfrage nicht beantwortet")
}

func TestActivityWhilePreparingToolCall(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	e.agent(0).emit(`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","contentIndex":0,"id":"call_1","toolName":"write"}}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := e.m.View(ctx, c.ID)
		if s, ok := e.m.pool.Get(v.SlotID); ok {
			if a := s.Info().Activity; a != nil && a.Kind == "preparing" && a.Tool == "write" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Tätigkeit „bereitet write vor“ nicht gesetzt")
}

func TestSendWithAttachments(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.AddInput(ctx, c.ID, "daten.csv", []byte("a\n")); err != nil {
		t.Fatal(err)
	}
	// Nicht vorhandener Anhang wird abgewiesen, ohne etwas zu senden.
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "Schau dir das an", []string{"gibtsnicht.csv"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbekannter Anhang: %v", err)
	}
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "Schau dir das an", []string{"daten.csv"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	var u struct {
		Content []struct{ Text string } `json:"content"`
	}
	_ = json.Unmarshal(msgs[0].Message, &u)
	want := "Schau dir das an\n\n[Anhänge unter /workspace/inputs/]\n- daten.csv"
	if u.Content[0].Text != want {
		t.Fatalf("Text an pi:\n%q\nerwartet\n%q", u.Content[0].Text, want)
	}
	// Nur Anhänge ohne Text sind erlaubt.
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "  ", []string{"daten.csv"}); err != nil {
		t.Fatalf("nur Anhänge: %v", err)
	}
}

// H1: Stirbt pi, lebt die Ausführungs-Sandbox noch; der Arbeitsbereich wird gesichert, bevor
// der Platz abgebaut wird. Der Chat ruht.
func TestPiDiesWorkspaceStillSaved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Hallo"})
	waitSettled(t, e, c.ID)
	a := e.agent(0)
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.wsSaves >= 1 })
	a.mu.Lock()
	before := a.wsSaves
	a.mu.Unlock()
	ch, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	a.crashPi()
	waitEvent(t, ch, "error", "")
	waitFor(t, func() bool { v, _ := e.m.View(ctx, c.ID); return v.State == store.StateDormant })
	a.mu.Lock()
	after := a.wsSaves
	a.mu.Unlock()
	if after != before+1 {
		t.Fatalf("Arbeitsbereich nach dem Tod von pi nicht gesichert: %d → %d", before, after)
	}
}

// H2: Stirbt die Ausführungs-Sandbox, behandelt der Manager das wie den Tod von pi: Fehler an
// die UI, Chat ruht, Sitzung (pi lebt noch) wird gesichert.
func TestExecSandboxDiesChatGoesDormant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Hallo"})
	waitSettled(t, e, c.ID)
	a := e.agent(0)
	ch, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	close(a.execDone)
	ev := waitEvent(t, ch, "error", "")
	if !strings.Contains(fmt.Sprint(ev.Data), "Ausführungs-Sandbox") {
		t.Fatalf("Meldung: %v", ev.Data)
	}
	waitFor(t, func() bool { v, _ := e.m.View(ctx, c.ID); return v.State == store.StateDormant && v.SlotID == "" })
	if sess, _ := e.st.LoadSession(ctx, c.ID); !strings.Contains(string(sess), `"agent":"a1"`) {
		t.Fatalf("Sitzung nicht gesichert: %q", sess)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("Bedingung nicht erfüllt")
}

// Ein Chat ohne Titel heißt nach der ersten Frage wie sie; /rename setzt den Titel fest.
func TestAutoTitleAndRename(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Title, "Neuer Chat ") {
		t.Fatalf("Platzhalter: %q", c.Title)
	}
	title := func() string {
		c, _ := e.st.GetChat(ctx, c.ID)
		return c.Title
	}
	if _, err := e.m.Send(ctx, c.ID, "  Wie  groß ist\nder Datensatz? "); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if got := title(); got != "Wie groß ist der Datensatz?" {
		t.Fatalf("nach der ersten Frage: %q", got)
	}
	if _, err := e.m.Send(ctx, c.ID, "Und die Klassen?"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if got := title(); got != "Wie groß ist der Datensatz?" {
		t.Fatalf("zweite Frage ändert den Titel: %q", got)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/rename"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("/rename ohne Namen: %v", err)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/rename  Schwanzbeißen: Klassen "); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "Schwanzbeißen: Klassen" {
		t.Fatalf("nach /rename: %q", got)
	}
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 4 {
		t.Fatalf("/rename erzeugt Nachrichten: %d", len(msgs))
	}

	// Vom Nutzer benannt: bleibt auch nach der ersten Frage.
	d, _ := e.m.Create(ctx, NewChat{Title: "Eigener Name"})
	if _, err := e.m.Send(ctx, d.ID, "Hallo"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, d.ID)
	if got, _ := e.st.GetChat(ctx, d.ID); got.Title != "Eigener Name" {
		t.Fatalf("eigener Titel überschrieben: %q", got.Title)
	}
}

type fakeTitler struct {
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func (f *fakeTitler) Title(_ context.Context, model, text string) (titler.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, model+"|"+text)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	return titler.Result{Title: "Datensatz: Größe", Model: model, Status: 200, Usage: config.Usage{Input: 100, Output: 5}, Cost: 0.00002, Started: time.Now()}, nil
}

// Der Modelltitel ersetzt die gekürzte erste Frage, einmal; sein Aufruf zählt nicht als llm_call.
// Benennt der Nutzer vorher um, bleibt sein Name.
func TestModelTitle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ft := &fakeTitler{}
	e.m.opt.Titler = ft
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.Send(ctx, c.ID, "Wie groß ist der Datensatz?"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitUntil(t, "Modelltitel", func() bool { got, _ := e.st.GetChat(ctx, c.ID); return got.Title == "Datensatz: Größe" })
	if _, err := e.m.Send(ctx, c.ID, "Und die Klassen?"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	ft.mu.Lock()
	calls := append([]string(nil), ft.calls...)
	ft.mu.Unlock()
	if len(calls) != 1 || calls[0] != "deepseek/deepseek-flash|Wie groß ist der Datensatz?" {
		t.Fatalf("Aufrufe des Titelgebers: %v", calls)
	}
	aux, _ := e.st.AuxCalls(ctx, c.ID)
	llm, _ := e.st.ListLLMCalls(ctx, c.ID)
	if len(aux) != 1 || aux[0].Purpose != "title" || aux[0].Cost != 0.00002 || aux[0].Input != 100 || len(llm) != 0 {
		t.Fatalf("aux_llm_calls %+v, llm_calls %d", aux, len(llm))
	}

	// Angelegt mit der ersten Frage: bekommt ebenfalls den Titel des Modells.
	m, err := e.m.Create(ctx, NewChat{Message: "Wie viele Bilder hat er?"})
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, m.ID)
	waitUntil(t, "Modelltitel beim Anlegen mit Nachricht", func() bool { got, _ := e.st.GetChat(ctx, m.ID); return got.Title == "Datensatz: Größe" })

	// Umbenennen, während der Titelgeber noch arbeitet: Der Name des Nutzers gewinnt.
	ft2 := &fakeTitler{release: make(chan struct{})}
	e.m.opt.Titler = ft2
	d, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.Send(ctx, d.ID, "Erste Frage"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "Titelgeber gerufen", func() bool { ft2.mu.Lock(); defer ft2.mu.Unlock(); return len(ft2.calls) == 1 })
	if _, err := e.m.Rename(ctx, d.ID, "Mein Name"); err != nil {
		t.Fatal(err)
	}
	close(ft2.release)
	waitUntil(t, "Aufruf erfasst", func() bool { a, _ := e.st.AuxCalls(ctx, d.ID); return len(a) == 1 })
	if got, _ := e.st.GetChat(ctx, d.ID); got.Title != "Mein Name" {
		t.Fatalf("Modelltitel überschreibt /rename: %q", got.Title)
	}
	waitSettled(t, e, d.ID)
}

// /model wechselt das Modell in pi und in der Datenbank; /effort setzt die Denkstufe und lehnt
// Stufen ab, die das Modell nicht kennt. Passt der Kontext nicht, ist der Wechsel gesperrt; mit
// compactFirst wird erst kompaktiert und dann gewechselt.
func TestModelAndEffort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	if v, _ := e.m.View(ctx, c.ID); v.ThinkingLevel != "high" || strings.Join(v.ThinkingLevels, ",") != "off,low,high,max" {
		t.Fatalf("nach dem Anlegen: %q %v", v.ThinkingLevel, v.ThinkingLevels)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/effort medium"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Stufe, die das Modell nicht kennt: %v", err)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/effort LOW"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.View(ctx, c.ID); v.ThinkingLevel != "low" || a.thinking != "low" {
		t.Fatalf("nach /effort: %q, pi %q", v.ThinkingLevel, a.thinking)
	}
	cmds, _ := e.m.Commands(ctx, c.ID)
	var effort, model Command
	for _, x := range cmds {
		switch x.Name {
		case "effort":
			effort = x
		case "model":
			model = x
		}
	}
	if len(effort.Options) != 4 || !effort.Options[1].Current || effort.Options[1].Label != "niedrig" {
		t.Fatalf("Vorschläge /effort: %+v", effort.Options)
	}
	if len(model.Options) != 2 || !model.Options[0].Current || model.Options[1].Value != "deepseek/klein" {
		t.Fatalf("Vorschläge /model: %+v", model.Options)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/model gibt/esnicht"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unbekanntes Modell: %v", err)
	}

	// Kontext 4200 passt in 8000: Wechsel sofort, die Denkstufe wird erneut gesetzt.
	if _, err := e.m.RunCommand(ctx, c.ID, "/model deepseek/klein"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.View(ctx, c.ID); v.Model != "deepseek/klein" || a.model != "deepseek/klein" || v.ThinkingLevel != "low" {
		t.Fatalf("nach /model: %q, pi %q, Stufe %q", v.Model, a.model, v.ThinkingLevel)
	}

	// Zurück zum großen, Kontext wachsen lassen, dann ist das kleine gesperrt.
	if _, err := e.m.SetModel(ctx, c.ID, "deepseek/deepseek-flash", false); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.ctxTokens = 9000
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "viel Text"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitUntil(t, "Kontext gemessen", func() bool {
		v, _ := e.m.View(ctx, c.ID)
		var u ContextUsage
		return json.Unmarshal(v.Context, &u) == nil && u.Tokens != nil && *u.Tokens == 9000
	})
	_, err = e.m.SetModel(ctx, c.ID, "deepseek/klein", false)
	var tooLarge *ContextTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Tokens != 9000 || tooLarge.Window != 8000 {
		t.Fatalf("zu voller Kontext: %v", err)
	}
	if v, _ := e.m.SetModel(ctx, c.ID, "deepseek/klein", true); v.PendingModel != "deepseek/klein" {
		t.Fatalf("vorgemerkt: %+v", v.PendingModel)
	}
	waitUntil(t, "nach der Kompaktierung gewechselt", func() bool {
		v, _ := e.m.View(ctx, c.ID)
		return v.Model == "deepseek/klein" && v.PendingModel == ""
	})
	if a.model != "deepseek/klein" {
		t.Fatalf("pi: %q", a.model)
	}
}

// piRun spielt einen Durchgang, den pi selbst beginnt (pi-subagents meldet einen fertigen Subagenten).
func (a *fakeAgent) piRun(note string) {
	a.emit(`{"type":"agent_start"}`)
	a.emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"custom","customType":"subagent-notify","display":true,"content":%q}}`, note))
	a.emit(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Ergebnis übernommen"}],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.001}}}}`)
	a.emit(`{"type":"agent_settled"}`)
}

// Beginnt pi einen Durchgang selbst, zählt er als Weckruf ohne Nutzer: eigene Zeile in chat_turns,
// die Meldung steht mit Herkunft system im Verlauf, und die Antwort gehört zu diesem Durchgang, nicht
// zum vorigen Auftrag des Nutzers.
func TestPiInitiatedTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "p"})
	if _, err := e.m.Send(ctx, c.ID, "starte Subagenten im Hintergrund"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	e.agent(0).piRun("Subagent fertig: Bericht liegt vor")
	waitUntil(t, "Durchgang von pi gespeichert", func() bool { m, _ := e.st.Messages(ctx, c.ID); return len(m) == 4 })
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	user, custom, answer := msgs[0], msgs[2], msgs[3]
	if custom.Role != "custom" || custom.Origin != store.OriginSystem || custom.Trigger != store.TriggerWake || custom.TurnID == nil {
		t.Fatalf("Meldung: %+v", custom)
	}
	if answer.TurnID == nil || user.TurnID == nil || *answer.TurnID != *custom.TurnID || *answer.TurnID == *user.TurnID || answer.Trigger != store.TriggerWake {
		t.Fatalf("Zuordnung der Antwort: Nutzer %v, Meldung %v, Antwort %v (%s)", user.TurnID, custom.TurnID, answer.TurnID, answer.Trigger)
	}
}

// Auch Durchgänge von pi unterliegen der Grenze für Durchgänge ohne Nutzer: darüber bricht der
// Orchestrator ab.
func TestPiInitiatedTurnLimited(t *testing.T) {
	e := setup(t)
	e.m.opt.AutoTurnsMax = 1
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "p"})
	if _, err := e.m.Send(ctx, c.ID, "los"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	a := e.agent(0)
	a.piRun("erste Meldung")
	waitSettled(t, e, c.ID)
	if strings.Count(strings.Join(a.commands(), ","), "abort") != 0 {
		t.Fatal("erster Durchgang ohne Nutzer abgebrochen")
	}
	a.piRun("zweite Meldung")
	waitUntil(t, "abgebrochen", func() bool { return strings.Count(strings.Join(a.commands(), ","), "abort") == 1 })
	if v, _ := e.m.View(ctx, c.ID); v.HoldReason != "" && v.HoldReason != HoldAutoTurns {
		t.Fatalf("Zurückhalten: %q", v.HoldReason)
	}
}

// Ohne Statusdatei nennt die Kind-Sitzung ihren Agenten in session_info („<agent>: <Auftrag>“).
func TestParseChildSessionAgentFromSessionInfo(t *testing.T) {
	data := `{"type":"session_info","id":"i1","name":"researcher: KONTEXT: Wir …"}` + "\n" +
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"Auftrag"}]}}` + "\n"
	es := parseChildSession("c", "r", "", data)
	if len(es) != 1 || es[0].Agent != "researcher" {
		t.Fatalf("Einträge: %+v", es)
	}
	if es := parseChildSession("c", "r", "scout", data); es[0].Agent != "scout" {
		t.Fatalf("bekannter Agent überschrieben: %+v", es)
	}
}

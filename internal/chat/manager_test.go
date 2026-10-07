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
	"agw/internal/toolset"
)

// fakeAgent plays pi: prompt produces a response including agent_settled.
type fakeAgent struct {
	id       string
	mu       sync.Mutex
	cmds     []map[string]any
	files    map[string][]byte
	internet bool
	closed   bool
	events   chan rpc.Event
	pollOut  string   // response to the subagents' read script
	execs    []string // executed commands
	reply    string   // text of the response (otherwise "Answer from …")

	wsOut        []byte // output of the backup script for /workspace (otherwise "unchanged")
	wsSaves      int    // calls of the backup script
	wsRestored   []byte // tar stream that arrived for restoring
	wsLatePrompt bool   // a prompt had already arrived at restore time

	execDone chan struct{} // execution sandbox ended (H2)

	hold       chan struct{} // set: the run only ends once the channel is closed
	failSwitch bool          // switch_session reports cancelled (resume fails)

	// onPrompt runs during the turn (after agent_start and the user message, before the
	// response); promptWait: for this long the prompt call does not respond (timeout after acceptance).
	onPrompt   func(msg string)
	promptWait time.Duration
	streaming  bool   // between prompt and agent_settled (get_state: isStreaming)
	onSwitch   func() // runs when the session is restored (resume)

	ctxTokens int64 // reported context (0: 4200); compact sets it to 2000

	withTool bool     // the run calls a tool (tool_execution_start before hold, _end after)
	steered  []string // steered requests (prompt with streamingBehavior steer), not inserted yet
	thinking string   // thinking level (get_state); set_thinking_level sets it
	model    string   // last set via set_model
}

func newFakeAgent(id string) *fakeAgent {
	return &fakeAgent{id: id, files: map[string][]byte{}, events: make(chan rpc.Event, 64), execDone: make(chan struct{})}
}

func (a *fakeAgent) ExecDone() <-chan struct{} { return a.execDone }

// crashPi ends pi's event stream without the slot being torn down (pi crash).
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
		// pi has written the session as soon as a response came.
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
		return rpc.Response{Success: true, Data: json.RawMessage(`{"commands":[{"name":"skill:artifacts","description":"Artifacts","source":"skill"}]}`)}, nil
	case "compact":
		a.mu.Lock()
		if a.ctxTokens != 0 {
			a.ctxTokens = 2000
		}
		a.mu.Unlock()
		go func() {
			a.emit(`{"type":"compaction_start","reason":"manual"}`)
			a.emit(`{"type":"compaction_end","reason":"manual","result":{"summary":"Summary","tokensBefore":9000,"estimatedTokensAfter":2000,"usage":{"input":9000,"output":300,"cacheRead":0,"totalTokens":9300,"cost":{"total":0.003}}},"aborted":false,"willRetry":false}`)
		}()
		return rpc.Response{Success: true, Data: json.RawMessage(`{"summary":"Summary"}`)}, nil
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
		// like pi: returns and removes what was steered in but not inserted yet (abort, by
		// contrast, continues with queued messages)
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
		answer := fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Answer from %s"}],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.001}}}}`, a.id)
		if a.reply != "" {
			answer = fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","responseId":"resp-%s","content":[{"type":"text","text":%q}],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.001}}}}`, a.id, a.reply)
		}
		onPrompt, wait := a.onPrompt, a.promptWait
		a.promptWait = 0 // only once
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
				// like pi: steered messages come after the tools, before the next model call
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

// ExecPi plays pi's container: only agw-exec and cat, no shell.
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
	return nil, errors.New("not executable in pi's container (no shell): " + strings.Join(cmd, " "))
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
	case strings.Contains(script, "realpath -e"): // read image: sh -c <script> sh <path> <limit>
		d, ok := a.files[cmd[4]]
		if !ok {
			return nil, errors.New("exec: exit code 3")
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
		return nil, errors.New("unknown exec: " + script)
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
	slotOf map[string]string // variant each sandbox was created for, by agent name

	failSwitch bool   // new sandboxes make switch_session fail
	onSwitch   func() // for new sandboxes: runs when the session is restored
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
		t.Skip("AGW_TEST_DATABASE_URL not set")
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
		if e.slotOf == nil {
			e.slotOf = map[string]string{}
		}
		e.slotOf[a.ContainerName()] = variant
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
			close(fa.events) // like a torn-down container: pi's stream ends
		}
	}
	p := pool.New[Agent](create, destroy, map[string]int{"cli": 1})
	p.SetKnown(toolset.Valid) // chats of other combinations get a slot on demand (issue #29)
	pctx, cancel := context.WithCancel(ctx)
	p.Start(pctx)
	t.Cleanup(func() { cancel(); p.Shutdown(context.Background()) })
	cat, _ := config.ParseCatalog([]byte(`{"default":"deepseek/deepseek-flash","providers":[{"id":"deepseek","models":[{"id":"deepseek-flash"},{"id":"klein","context_window":8000}]}]}`))
	e.p, e.cat = p, cat
	e.opt = Options{ApprovalTimeout: 2 * time.Second, ArtifactMaxBytes: 1 << 20, AcquireTimeout: 3 * time.Second, AutoCompactDefault: true, CompactReserveTokens: 16384, CompactKeepRecent: 20000, MaxSubagents: config.DefaultMaxSubagents}
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
			t.Fatalf("event %s/%s did not arrive", kind, piType)
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
	t.Fatal("run did not finish")
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
		t.Fatalf("created: %+v", c)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if _, err := e.m.Send(ctx, c.ID, "Hello"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "pi", "message_update")
	waitEvent(t, events, "pi", "agent_settled")
	waitSettled(t, e, c.ID)

	a := e.agent(0)
	if !a.internet {
		t.Fatal("internet not set")
	}
	cmds := strings.Join(a.commands(), ",")
	if i, j := strings.Index(cmds, "set_model"), strings.Index(cmds, "prompt"); i != 0 || j < i {
		t.Fatalf("commands: %s", cmds)
	}
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("messages: %+v", msgs)
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.Tokens.Total != 15 || v.Cost < 0.0009 {
		t.Fatalf("cost/tokens: %+v %v", v.Tokens, v.Cost)
	}
	// The session is saved in the background after agent_settled (Review H1).
	var sess []byte
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if sess, _ = e.st.LoadSession(ctx, c.ID); strings.Contains(string(sess), `"agent":"a1"`) {
			break
		}
	}
	if !strings.Contains(string(sess), `"agent":"a1"`) {
		t.Fatalf("session not saved: %q", sess)
	}
}

func TestSuspendAndResumeInFreshSandbox(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Remember 42"})
	waitSettled(t, e, c.ID)
	v, err := e.m.Suspend(ctx, c.ID)
	if err != nil || v.State != store.StateDormant || v.SlotID != "" {
		t.Fatalf("Suspend: %+v %v", v, err)
	}
	resumed, err := e.m.Send(ctx, c.ID, "What was the number?")
	if err != nil || !resumed.Resumed {
		t.Fatalf("resume: %v %v", resumed, err)
	}
	waitSettled(t, e, c.ID)
	// The new agent is not the old one (single use) and has received the session.
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
		t.Fatal("no fresh sandbox with switch_session")
	}
	p := "/agent/sessions/" + c.ID + ".jsonl"
	if got := string(fresh.files[p]); !strings.Contains(got, `"agent":"a1"`) {
		t.Fatalf("session not restored: %q", got)
	}
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 4 {
		t.Fatalf("messages after resume: %d", len(msgs))
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
		r, err := e.m.Upload(store.WithToolCall(ctx, "call_up"), c.ID, slot, "cli", "numbers.csv", 4, "", strings.NewReader("1,2\n"))
		done <- result{r.Status, err}
	}()
	ev := waitEvent(t, events, "approval", "")
	ap := ev.Data.(store.Approval)
	if ap.State != "pending" || ap.Preview != "1,2\n" {
		t.Fatalf("request: %+v", ap)
	}
	info, _ := e.m.pool.Get(slot)
	if info.Info().Activity.Kind != "waiting_approval" {
		t.Fatalf("activity: %+v", info.Info().Activity)
	}
	if _, err := e.m.Suspend(ctx, c.ID); !errors.Is(err, ErrPendingApproval) {
		t.Fatalf("idle despite pending approval: %v", err)
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
		t.Fatalf("artifacts: %+v", arts)
	}
	for _, k := range e.blobs.keys() {
		if strings.HasPrefix(k, "pending/") {
			t.Fatalf("pending object left behind: %s", k)
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
		t.Fatalf("rejection does not arrive: %s", s)
	}
	if len(e.blobs.keys()) != 0 {
		t.Fatalf("objects left: %v", e.blobs.keys())
	}
}

func TestUploadTimeoutExpires(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 50 * time.Millisecond
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	r, err := e.m.Upload(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", "a.txt", 1, "", strings.NewReader("a"))
	if err != nil || r.Status != "rejected" || !strings.Contains(r.Message, "waiting time") {
		t.Fatalf("expiry: %+v %v", r, err)
	}
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if len(aps) != 1 || aps[0].State != "expired" {
		t.Fatalf("state: %+v", aps)
	}
}

func TestInternetToggleAndInputs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	off := false
	c, _ := e.m.Create(ctx, NewChat{Internet: &off})
	a := e.agent(0)
	if a.internet {
		t.Fatal("internet on although off was requested")
	}
	if _, err := e.m.SetInternet(ctx, c.ID, true); err != nil || !a.internet {
		t.Fatalf("toggle: %v %v", err, a.internet)
	}
	if _, err := e.m.AddInput(ctx, c.ID, "../data.csv", []byte("x,y\n")); err != nil {
		t.Fatal(err)
	}
	if string(a.files["/workspace/inputs/data.csv"]) != "x,y\n" {
		t.Fatalf("input not mirrored: %v", a.files)
	}
	// After resuming, the input is also in the new sandbox.
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, c.ID, "continue"); err != nil {
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
	if string(fresh.files["/workspace/inputs/data.csv"]) != "x,y\n" || !fresh.internet {
		t.Fatalf("fresh sandbox: files %v, internet %v", fresh.files, fresh.internet)
	}
}

func TestUnknownModelAndVariant(t *testing.T) {
	e := setup(t)
	if _, err := e.m.Create(context.Background(), NewChat{Model: "x/y"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("model: %v", err)
	}
	if _, err := e.m.Create(context.Background(), NewChat{Variant: "shell"}); !errors.Is(err, ErrUnknownVariant) {
		t.Fatalf("variant: %v", err)
	}
}

// Issue #29: new chats get the configured combination; a variant in the request must name the same
// one, and chats stored with another combination or the older id "both" still resume.
func TestToolsetsFixedForNewChats(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if e.m.Options().Toolsets.Key() != "cli" {
		t.Fatalf("default combination: %q", e.m.Options().Toolsets.Key())
	}
	for _, v := range []string{"", "cli", " CLI "} {
		c, err := e.m.Create(ctx, NewChat{Variant: v})
		if err != nil {
			t.Fatalf("variant %q: %v", v, err)
		}
		if c.Variant != "cli" {
			t.Fatalf("variant %q stored as %q", v, c.Variant)
		}
		_, _ = e.m.Suspend(ctx, c.ID)
	}
	for _, v := range []string{"mcp", "both", "cli,api"} {
		if _, err := e.m.Create(ctx, NewChat{Variant: v}); !errors.Is(err, ErrVariantFixed) || !strings.Contains(err.Error(), `new chats get "cli"`) {
			t.Fatalf("variant %q: %v", v, err)
		}
	}

	// Older chats: stored variants of before and another combination resume in a slot of their
	// own combination (started on demand), "both" as cli,mcp.
	for stored, key := range map[string]string{"both": "cli,mcp", "mcp": "mcp", "api": "api", "cli,api": "cli,api"} {
		old, err := e.st.CreateChat(ctx, store.NewChat{Title: "old " + stored, Model: "deepseek/deepseek-flash", Variant: stored})
		if err != nil {
			t.Fatal(err)
		}
		_ = e.st.SetState(ctx, old.ID, store.StateDormant)
		if _, err := e.m.Send(ctx, old.ID, "hello"); err != nil {
			t.Fatalf("%s: %v", stored, err)
		}
		waitSettled(t, e, old.ID)
		e.m.mu.Lock()
		l := e.m.live[old.ID]
		e.m.mu.Unlock()
		if l == nil {
			t.Fatalf("%s: not resumed", stored)
		}
		e.mu.Lock()
		got := e.slotOf[l.slot.Worker.ContainerName()]
		e.mu.Unlock()
		if got != key || l.slot.Variant != key {
			t.Fatalf("%s: slot of %q (pool key %q), want %q", stored, got, l.slot.Variant, key)
		}
		if v, _ := e.m.View(ctx, old.ID); v.Variant != stored {
			t.Fatalf("%s: stored variant changed to %q", stored, v.Variant)
		}
	}
}

// A gateway configured with a combination gives it to every new chat.
func TestToolsetsConfigured(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ts, _ := toolset.Parse("api,cli")
	opt := e.opt
	opt.Toolsets = ts
	m := NewManager(e.st, e.p, e.cat, e.blobs, artifacts.NewBroker(), opt)
	for _, v := range []string{"", "cli,api", "api,cli"} {
		c, err := m.Create(ctx, NewChat{Variant: v})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if c.Variant != "cli,api" {
			t.Fatalf("%q stored as %q", v, c.Variant)
		}
		_, _ = m.Suspend(ctx, c.ID)
	}
	if _, err := m.Create(ctx, NewChat{Variant: "cli"}); !errors.Is(err, ErrVariantFixed) {
		t.Fatalf("cli on a cli,api gateway: %v", err)
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
	// Tuesday 07:00 UTC (peak) and 12:00 UTC (off-peak), 1M output tokens each.
	peakMsg := json.RawMessage(`{"role":"assistant","provider":"deepseek","model":"deepseek-flash","timestamp":1790665200000,"usage":{"output":1000000}}`)
	offMsg := json.RawMessage(`{"role":"assistant","provider":"deepseek","model":"deepseek-flash","timestamp":1790683200000,"usage":{"output":1000000}}`)
	b1, b2 := m.bill(context.Background(), "", peakMsg), m.bill(context.Background(), "", offMsg)
	if b1 == nil || !b1.Peak || b1.Cost != 1.2 {
		t.Fatalf("peak: %+v", b1)
	}
	if b2 == nil || b2.Peak || b2.Cost != 0.6 {
		t.Fatalf("off-peak: %+v", b2)
	}
}

func TestContextAfterSettle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "hello"})
	waitSettled(t, e, c.ID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := e.m.View(ctx, c.ID)
		if v.Context != nil {
			var u ContextUsage
			_ = json.Unmarshal(v.Context, &u)
			if u.Tokens == nil || *u.Tokens != 4200 || u.Window != 1000000 || u.ThresholdTokens != int64(1000000-e.m.opt.CompactReserveTokens) {
				t.Fatalf("context: %+v", u)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("context not saved")
}

func TestCompactCommandStoresCompaction(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "hello"})
	waitSettled(t, e, c.ID)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if _, err := e.m.RunCommand(ctx, c.ID, "/compact focus on numbers"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "pi", "compaction_end")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := e.m.View(ctx, c.ID)
		if v.Compactions == 1 {
			msgs, _ := e.st.Messages(ctx, c.ID)
			last := msgs[len(msgs)-1]
			if last.Role != "compaction" || !strings.Contains(string(last.Message), "Summary") {
				t.Fatalf("entry: %+v", last)
			}
			var found bool
			for _, cmd := range e.agent(0).cmds {
				if cmd["type"] == "compact" && cmd["customInstructions"] == compactLanguageHint+" focus on numbers" {
					found = true
				}
			}
			if !found {
				t.Fatal("compact sent without instructions")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("compaction not saved")
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
		t.Fatal("automatic compaction still on")
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
		t.Fatalf("set_auto_compaction not off: %v %v", seen, last)
	}
	_, _ = e.m.Suspend(ctx, c.ID)
	_, _ = e.m.Send(ctx, c.ID, "continue")
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
		t.Fatalf("fresh sandbox did not get the setting: %v %v", seen, last)
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
		t.Fatalf("commands: %v", names)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/skill:artifacts list"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	if !strings.Contains(string(msgs[0].Message), "/skill:artifacts list") {
		t.Fatalf("not passed on as a prompt: %s", msgs[0].Message)
	}
	// After idling the list stays known.
	_, _ = e.m.Suspend(ctx, c.ID)
	cmds, _ = e.m.Commands(ctx, c.ID)
	if len(cmds) < 3 {
		t.Fatalf("list after idling: %+v", cmds)
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
		r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", "I need to install a Python library.")
		done <- r.Status
	}()
	ev := waitEvent(t, events, "approval", "")
	ap := ev.Data.(store.Approval)
	if ap.Kind != "internet_access" || !strings.Contains(ap.Name, "library") {
		t.Fatalf("request: %+v", ap)
	}
	if e.agent(0).internet {
		t.Fatal("internet on before the decision")
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if s := <-done; s != "approved" {
		t.Fatalf("status: %s", s)
	}
	v, _ := e.m.View(ctx, c.ID)
	if !e.agent(0).internet || !v.Internet {
		t.Fatal("internet not on after approval")
	}
	// Already on: approved immediately, without a new request.
	r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", "again")
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if r.Status != "approved" || len(aps) != 1 {
		t.Fatalf("second request: %+v, %d approvals", r, len(aps))
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
		r, _ := e.m.RequestInternet(ctx, c.ID, slot, "mcp", "Fetching a web page")
		done <- r.Status
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	_, _ = e.m.Decide(ctx, ap.ID, false)
	if s := <-done; s != "rejected" {
		t.Fatalf("status: %s", s)
	}
	if e.agent(0).internet {
		t.Fatal("internet on despite rejection")
	}
}

// After idling, pi's stream ends because the container is torn down.
// That must not count as a sandbox crash and must not trigger detach twice
// (previously: "close of closed channel" and a crash of the orchestrator).
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
			t.Fatalf("chat active again after idling: %+v", c)
		}
	}
}

// M7: if the user decides exactly when the waiting time expires, the decision
// from the database applies.
func TestDecisionWinsOverExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	ap, _ := e.st.CreateApproval(ctx, store.Approval{ChatID: c.ID, Kind: "artifact_upload", Via: "cli", Name: "x", ContentType: "text/plain", PendingKey: "p"})
	if _, _, err := e.st.DecideApproval(ctx, ap.ID, store.ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	if got := e.m.settle(ctx, c.ID, ap.ID, false, artifacts.ErrTimeout); got != store.ApprovalApproved {
		t.Fatalf("state: %s", got)
	}
}

// H3: toggling internet waits until a running resume is finished, and
// then takes effect on the new sandbox.
func TestSetInternetDuringResume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	on := true
	c, _ := e.m.Create(ctx, NewChat{Internet: &on, Message: "hello"})
	waitSettled(t, e, c.ID)
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = e.m.Send(ctx, c.ID, "continue"); close(done) }()
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
		t.Fatalf("internet: DB %v, sandbox %v – both must be off", v.Internet, last.internet)
	}
}

func TestAttributeAndRecord(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	a := e.agent(0)
	att := e.m.Attribute(a.IP())
	if att.ChatID != c.ID || att.MaxConcurrent != 6 {
		t.Fatalf("attribution: %+v", att)
	}
	if got := e.m.Attribute("10.9.9.9"); got.ChatID != "" {
		t.Fatalf("foreign address attributed: %+v", got)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	e.m.Record(llmproxy.Call{ChatID: c.ID, SlotID: att.SlotID, SourceIP: a.IP(), Model: "deepseek/deepseek-flash", ResponseID: "r1", Status: 200,
		Usage: config.Usage{Input: 10, Output: 5}, Cost: 0.002, StartedAt: time.Now(), ToolCalls: []llmproxy.ToolCall{{Name: "bash", Arguments: "{}"}}})
	waitEvent(t, events, "llm_call", "")
	v, _ := e.m.View(ctx, c.ID)
	if v.LLMCalls != 1 || v.Cost != 0.002 || v.CostOther != 0.002 {
		t.Fatalf("after call: %+v", v.Chat)
	}
	// After idling the address belongs to no chat anymore.
	_, _ = e.m.Suspend(ctx, c.ID)
	if got := e.m.Attribute(a.IP()); got.ChatID != "" {
		t.Fatalf("address still attributed after idling: %+v", got)
	}
}

// The limit is fixed for the service (issue #24): a value in the request has no effect, and a chat
// that stored another value before runs with the service's limit after resuming.
func TestSubagentLimitFixedForService(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	one := 1
	c, err := e.m.Create(ctx, NewChat{MaxSubagents: &one})
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSubagents != 5 {
		t.Fatalf("value from the request applied: %d", c.MaxSubagents)
	}
	if att := e.m.Attribute(e.agent(0).IP()); att.MaxConcurrent != 6 {
		t.Fatalf("proxy limit: %+v", att)
	}
	a := e.agent(0)
	a.mu.Lock()
	cfg := string(a.files["/agent/config/extensions/subagent/config.json"])
	a.mu.Unlock()
	if !strings.Contains(cfg, `"globalConcurrencyLimit":5`) || !strings.Contains(cfg, `"maxActiveAsyncRunsPerSession":5`) || strings.Contains(cfg, "maxSubagentSpawnsPerSession") {
		t.Fatalf("pi-subagents configuration (at the same time, no cap in total): %s", cfg)
	}
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	// A chat from before issue #24 with its own value: view and resumed sandbox use the service's.
	old, err := e.st.CreateChat(ctx, store.NewChat{Title: "old", Model: "deepseek/deepseek-flash", Variant: "cli", MaxSubagents: 2})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.st.SetState(ctx, old.ID, store.StateDormant)
	if v, _ := e.m.View(ctx, old.ID); v.MaxSubagents != 5 {
		t.Fatalf("old chat shows its stored value: %d", v.MaxSubagents)
	}
	if _, err := e.m.Send(ctx, old.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, old.ID)
	e.m.mu.Lock()
	l := e.m.live[old.ID]
	e.m.mu.Unlock()
	if l == nil || l.maxSub != 5 {
		t.Fatalf("old chat not running with the service's limit: %+v", l)
	}
	if att := e.m.Attribute(l.ip); att.MaxConcurrent != 6 {
		t.Fatalf("proxy limit of the old chat: %+v", att)
	}
	if list, _ := e.m.List(ctx); len(list) == 0 || list[0].MaxSubagents != 5 || list[len(list)-1].MaxSubagents != 5 {
		t.Fatalf("list: %+v", list)
	}
}

// subRun: one subagent run as the poll sees it; state "" means no status file (foreground run).
type subRun struct {
	id, state string
	done      bool // the session ends with a text answer
}

func runID(i int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", i) }

func pollRuns(runs ...subRun) string {
	type file struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Data   string `json:"data"`
	}
	fs := []file{}
	agents := map[string]string{}
	status := map[string]any{}
	for _, r := range runs {
		d := `{"type":"message","id":"u` + r.id + `","message":{"role":"user","content":[{"type":"text","text":"Task ` + r.id + `"}]}}` + "\n" +
			`{"type":"message","id":"m` + r.id + `","message":{"role":"assistant","responseId":"resp-` + r.id + `","content":[{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}]}}` + "\n"
		if r.done {
			d += `{"type":"message","id":"t` + r.id + `","message":{"role":"assistant","responseId":"resp2-` + r.id + `","content":[{"type":"text","text":"done"}]}}` + "\n"
		}
		fs = append(fs, file{Path: "/agent/sessions/s/" + r.id + "/run-0/session.jsonl", Offset: int64(len(d)), Data: d})
		agents[r.id] = "scout"
		if r.state != "" {
			status[r.id] = map[string]any{"agent": "scout", "label": "search", "state": r.state}
		}
	}
	b, _ := json.Marshal(map[string]any{"files": fs, "agents": agents, "runs": status})
	return string(b)
}

// poll runs one round of the monitoring with the given runs and returns whether it aborted.
func (e *env) poll(t *testing.T, chatID string, runs ...subRun) bool {
	t.Helper()
	a := e.agent(0)
	a.mu.Lock()
	a.pollOut = pollRuns(runs...)
	before := len(a.execs)
	a.mu.Unlock()
	e.m.mu.Lock()
	l := e.m.live[chatID]
	e.m.mu.Unlock()
	e.m.pollSubagents(chatID, l, map[string]int64{}, map[string]runInfo{}, map[string]string{})
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Contains(strings.Join(a.execs[before:], "\n"), "pi: agw-exec kill-node")
}

func limitCalls(t *testing.T, e *env, chatID string) []string {
	t.Helper()
	calls, err := e.st.ListSocketCalls(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range calls {
		if c.Op == "subagent_limit" {
			out = append(out, c.Detail)
		}
	}
	return out
}

// Five subagents at the same time are allowed, the sixth running one aborts the turn; the runs ended
// that way never count again.
func TestSubagentLimitCountsConcurrentRuns(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()

	var runs []subRun
	for i := 1; i <= 5; i++ {
		runs = append(runs, subRun{id: runID(i), state: "running"})
	}
	if e.poll(t, c.ID, runs...) {
		t.Fatal("aborted with five subagents at the same time")
	}
	if r := waitEvent(t, events, "subagent_run", "").Data.(store.SubagentRun); r.Label != "search" || r.State != "running" {
		t.Fatalf("run: %+v", r)
	}
	if se := waitEvent(t, events, "subagent", "").Data.(store.SubagentEntry); se.Agent != "scout" {
		t.Fatalf("entry: %+v", se)
	}
	if v, _ := e.m.View(ctx, c.ID); v.SubagentsRunning != 5 || v.Subagents != 5 || v.MaxSubagents != 5 {
		t.Fatalf("view with five running: running %d, started %d, max %d", v.SubagentsRunning, v.Subagents, v.MaxSubagents)
	}
	// Queued runs wait for capacity in pi-subagents and do not count.
	if e.poll(t, c.ID, append(runs, subRun{id: runID(6), state: "queued"})...) {
		t.Fatal("aborted because of a queued run")
	}
	runs = append(runs, subRun{id: runID(6), state: "running"})
	if !e.poll(t, c.ID, runs...) {
		t.Fatal("no abort with six subagents at the same time")
	}
	waitEvent(t, events, "error", "")
	a := e.agent(0)
	a.mu.Lock()
	aborted := strings.Contains(fmt.Sprint(a.cmds), "abort")
	a.mu.Unlock()
	if !aborted {
		t.Fatal("main agent not aborted")
	}
	if got := limitCalls(t, e, c.ID); len(got) != 1 || got[0] != "6 running at the same time, 5 allowed" {
		t.Fatalf("socket calls: %q", got)
	}
	// The killed runs still say "running" in their stale status files: no second intervention, and
	// a new run starts from zero.
	if e.poll(t, c.ID, append(runs, subRun{id: runID(7), state: "running"})...) {
		t.Fatal("killed runs counted again")
	}
	if v, _ := e.m.View(ctx, c.ID); v.SubagentsRunning != 1 || v.Subagents != 7 {
		t.Fatalf("after the intervention: running %d, started %d", v.SubagentsRunning, v.Subagents)
	}
}

// The limit counts subagents at the same time, not in total: eight runs one after another are fine.
func TestSubagentLimitIgnoresFinishedRuns(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	var runs []subRun
	for i := 1; i <= 8; i++ {
		for j := range runs {
			runs[j].state = "complete"
		}
		runs = append(runs, subRun{id: runID(i), state: "running"})
		if e.poll(t, c.ID, runs...) {
			t.Fatalf("aborted at run %d although only one runs at a time", i)
		}
	}
	if got := limitCalls(t, e, c.ID); len(got) != 0 {
		t.Fatalf("socket calls: %q", got)
	}
	if v, _ := e.m.View(ctx, c.ID); v.SubagentsRunning != 1 || v.Subagents != 8 {
		t.Fatalf("view: running %d, started %d", v.SubagentsRunning, v.Subagents)
	}
}

func TestExtensionUIIsCancelled(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	a := e.agent(0)
	a.emit(`{"type":"extension_ui_request","id":"ui-1","method":"confirm","title":"Allow more subagents?"}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for _, cmd := range a.cmds {
			if cmd["type"] == "extension_ui_response" && cmd["id"] == "ui-1" && cmd["cancelled"] == true {
				a.mu.Unlock()
				calls, _ := e.m.SocketCalls(ctx, c.ID)
				if len(calls) == 0 || calls[len(calls)-1].Op != "extension_ui" {
					t.Fatalf("not logged: %+v", calls)
				}
				return
			}
		}
		a.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("query not answered")
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
	t.Fatal(`activity "preparing write" not set`)
}

func TestSendWithAttachments(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.AddInput(ctx, c.ID, "data.csv", []byte("a\n")); err != nil {
		t.Fatal(err)
	}
	// A missing attachment is refused without sending anything.
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "Have a look at this", []string{"doesnotexist.csv"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown attachment: %v", err)
	}
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "Have a look at this", []string{"data.csv"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	var u struct {
		Content []struct{ Text string } `json:"content"`
	}
	_ = json.Unmarshal(msgs[0].Message, &u)
	want := "Have a look at this\n\n[Attachments in /workspace/inputs/]\n- data.csv"
	if u.Content[0].Text != want {
		t.Fatalf("text to pi:\n%q\nexpected\n%q", u.Content[0].Text, want)
	}
	// Attachments only, without text, are allowed.
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "  ", []string{"data.csv"}); err != nil {
		t.Fatalf("attachments only: %v", err)
	}
}

// H1: if pi dies, the execution sandbox is still alive; the workspace is saved before
// the slot is torn down. The chat goes idle.
func TestPiDiesWorkspaceStillSaved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Hello"})
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
		t.Fatalf("workspace not saved after pi died: %d → %d", before, after)
	}
}

// H2: if the execution sandbox dies, the manager treats it like pi dying: error to
// the UI, chat goes idle, session (pi is still alive) is saved.
func TestExecSandboxDiesChatGoesDormant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Hello"})
	waitSettled(t, e, c.ID)
	a := e.agent(0)
	ch, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	close(a.execDone)
	ev := waitEvent(t, ch, "error", "")
	if !strings.Contains(fmt.Sprint(ev.Data), "execution sandbox") {
		t.Fatalf("message: %v", ev.Data)
	}
	waitFor(t, func() bool { v, _ := e.m.View(ctx, c.ID); return v.State == store.StateDormant && v.SlotID == "" })
	if sess, _ := e.st.LoadSession(ctx, c.ID); !strings.Contains(string(sess), `"agent":"a1"`) {
		t.Fatalf("session not saved: %q", sess)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

// A chat without a title is named after the first question; /rename fixes the title.
func TestAutoTitleAndRename(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Title, "New chat ") {
		t.Fatalf("placeholder: %q", c.Title)
	}
	title := func() string {
		c, _ := e.st.GetChat(ctx, c.ID)
		return c.Title
	}
	if _, err := e.m.Send(ctx, c.ID, "  How  big is\nthe dataset? "); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if got := title(); got != "How big is the dataset?" {
		t.Fatalf("after the first question: %q", got)
	}
	if _, err := e.m.Send(ctx, c.ID, "And the classes?"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if got := title(); got != "How big is the dataset?" {
		t.Fatalf("second question changes the title: %q", got)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/rename"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("/rename without a name: %v", err)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/rename  Tail biting: classes "); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "Tail biting: classes" {
		t.Fatalf("after /rename: %q", got)
	}
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 4 {
		t.Fatalf("/rename creates messages: %d", len(msgs))
	}

	// Named by the user: stays even after the first question.
	d, _ := e.m.Create(ctx, NewChat{Title: "Own name"})
	if _, err := e.m.Send(ctx, d.ID, "Hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, d.ID)
	if got, _ := e.st.GetChat(ctx, d.ID); got.Title != "Own name" {
		t.Fatalf("own title overwritten: %q", got.Title)
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
	return titler.Result{Title: "Dataset: size", Model: model, Status: 200, Usage: config.Usage{Input: 100, Output: 5}, Cost: 0.00002, Started: time.Now()}, nil
}

// The model title replaces the shortened first question, once; its call does not count as llm_call.
// If the user renames the chat before, their name stays.
func TestModelTitle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ft := &fakeTitler{}
	e.m.opt.Titler = ft
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.Send(ctx, c.ID, "How big is the dataset?"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitUntil(t, "model title", func() bool { got, _ := e.st.GetChat(ctx, c.ID); return got.Title == "Dataset: size" })
	if _, err := e.m.Send(ctx, c.ID, "And the classes?"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	ft.mu.Lock()
	calls := append([]string(nil), ft.calls...)
	ft.mu.Unlock()
	if len(calls) != 1 || calls[0] != "deepseek/deepseek-flash|How big is the dataset?" {
		t.Fatalf("calls of the titler: %v", calls)
	}
	aux, _ := e.st.AuxCalls(ctx, c.ID)
	llm, _ := e.st.ListLLMCalls(ctx, c.ID)
	if len(aux) != 1 || aux[0].Purpose != "title" || aux[0].Cost != 0.00002 || aux[0].Input != 100 || len(llm) != 0 {
		t.Fatalf("aux_llm_calls %+v, llm_calls %d", aux, len(llm))
	}

	// Created with the first question: also gets the model's title.
	m, err := e.m.Create(ctx, NewChat{Message: "How many images does it have?"})
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, m.ID)
	waitUntil(t, "model title when created with a message", func() bool { got, _ := e.st.GetChat(ctx, m.ID); return got.Title == "Dataset: size" })

	// Renaming while the titler is still working: the user's name wins.
	ft2 := &fakeTitler{release: make(chan struct{})}
	e.m.opt.Titler = ft2
	d, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.Send(ctx, d.ID, "First question"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "titler called", func() bool { ft2.mu.Lock(); defer ft2.mu.Unlock(); return len(ft2.calls) == 1 })
	if _, err := e.m.Rename(ctx, d.ID, "My name"); err != nil {
		t.Fatal(err)
	}
	close(ft2.release)
	waitUntil(t, "call recorded", func() bool { a, _ := e.st.AuxCalls(ctx, d.ID); return len(a) == 1 })
	if got, _ := e.st.GetChat(ctx, d.ID); got.Title != "My name" {
		t.Fatalf("model title overwrites /rename: %q", got.Title)
	}
	waitSettled(t, e, d.ID)
}

// /model switches the model in pi and in the database; /effort sets the thinking level and refuses
// levels the model does not know. If the context does not fit, the switch is blocked; with
// compactFirst it compacts first and then switches.
func TestModelAndEffort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	if v, _ := e.m.View(ctx, c.ID); v.ThinkingLevel != "high" || strings.Join(v.ThinkingLevels, ",") != "off,low,high,max" {
		t.Fatalf("after creating: %q %v", v.ThinkingLevel, v.ThinkingLevels)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/effort medium"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("level the model does not know: %v", err)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/effort LOW"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.View(ctx, c.ID); v.ThinkingLevel != "low" || a.thinking != "low" {
		t.Fatalf("after /effort: %q, pi %q", v.ThinkingLevel, a.thinking)
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
	if len(effort.Options) != 4 || !effort.Options[1].Current || effort.Options[1].Label != "low" {
		t.Fatalf("suggestions /effort: %+v", effort.Options)
	}
	if len(model.Options) != 2 || !model.Options[0].Current || model.Options[1].Value != "deepseek/klein" {
		t.Fatalf("suggestions /model: %+v", model.Options)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/model does/notexist"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}

	// Context 4200 fits into 8000: switch immediately, the thinking level is set again.
	if _, err := e.m.RunCommand(ctx, c.ID, "/model deepseek/klein"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.View(ctx, c.ID); v.Model != "deepseek/klein" || a.model != "deepseek/klein" || v.ThinkingLevel != "low" {
		t.Fatalf("after /model: %q, pi %q, level %q", v.Model, a.model, v.ThinkingLevel)
	}

	// Back to the large one, let the context grow, then the small one is blocked.
	if _, err := e.m.SetModel(ctx, c.ID, "deepseek/deepseek-flash", false); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.ctxTokens = 9000
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "lots of text"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitUntil(t, "context measured", func() bool {
		v, _ := e.m.View(ctx, c.ID)
		var u ContextUsage
		return json.Unmarshal(v.Context, &u) == nil && u.Tokens != nil && *u.Tokens == 9000
	})
	_, err = e.m.SetModel(ctx, c.ID, "deepseek/klein", false)
	var tooLarge *ContextTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Tokens != 9000 || tooLarge.Window != 8000 {
		t.Fatalf("context too full: %v", err)
	}
	if v, _ := e.m.SetModel(ctx, c.ID, "deepseek/klein", true); v.PendingModel != "deepseek/klein" {
		t.Fatalf("scheduled: %+v", v.PendingModel)
	}
	waitUntil(t, "switched after the compaction", func() bool {
		v, _ := e.m.View(ctx, c.ID)
		return v.Model == "deepseek/klein" && v.PendingModel == ""
	})
	if a.model != "deepseek/klein" {
		t.Fatalf("pi: %q", a.model)
	}
}

// piRun plays a turn that pi starts itself (pi-subagents reports a finished subagent).
func (a *fakeAgent) piRun(note string) {
	a.emit(`{"type":"agent_start"}`)
	a.emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"custom","customType":"subagent-notify","display":true,"content":%q}}`, note))
	a.emit(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Result taken over"}],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.001}}}}`)
	a.emit(`{"type":"agent_settled"}`)
}

// If pi starts a turn itself, it counts as a wake-up without the user: its own row in chat_turns,
// the note appears in the history with origin system, and the response belongs to this turn, not
// to the user's previous request.
func TestPiInitiatedTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "p"})
	if _, err := e.m.Send(ctx, c.ID, "start subagents in the background"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	e.agent(0).piRun("Subagent finished: report is ready")
	waitUntil(t, "turn from pi saved", func() bool { m, _ := e.st.Messages(ctx, c.ID); return len(m) == 4 })
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	user, custom, answer := msgs[0], msgs[2], msgs[3]
	if custom.Role != "custom" || custom.Origin != store.OriginSystem || custom.Trigger != store.TriggerWake || custom.TurnID == nil {
		t.Fatalf("note: %+v", custom)
	}
	if answer.TurnID == nil || user.TurnID == nil || *answer.TurnID != *custom.TurnID || *answer.TurnID == *user.TurnID || answer.Trigger != store.TriggerWake {
		t.Fatalf("attribution of the response: user %v, note %v, response %v (%s)", user.TurnID, custom.TurnID, answer.TurnID, answer.Trigger)
	}
}

// Turns from pi are also subject to the limit for turns without the user: above it the
// orchestrator aborts.
func TestPiInitiatedTurnLimited(t *testing.T) {
	e := setup(t)
	e.m.opt.AutoTurnsMax = 1
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "p"})
	if _, err := e.m.Send(ctx, c.ID, "go"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	a := e.agent(0)
	a.piRun("first note")
	waitSettled(t, e, c.ID)
	if strings.Count(strings.Join(a.commands(), ","), "abort") != 0 {
		t.Fatal("first turn without the user aborted")
	}
	a.piRun("second note")
	waitUntil(t, "aborted", func() bool { return strings.Count(strings.Join(a.commands(), ","), "abort") == 1 })
	if v, _ := e.m.View(ctx, c.ID); v.HoldReason != "" && v.HoldReason != HoldAutoTurns {
		t.Fatalf("hold back: %q", v.HoldReason)
	}
}

// Without a status file the child session names its agent in session_info ("<agent>: <task>").
func TestParseChildSessionAgentFromSessionInfo(t *testing.T) {
	data := `{"type":"session_info","id":"i1","name":"researcher: CONTEXT: We …"}` + "\n" +
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"Task"}]}}` + "\n"
	es := parseChildSession("c", "r", "", data)
	if len(es) != 1 || es[0].Agent != "researcher" {
		t.Fatalf("entries: %+v", es)
	}
	if es := parseChildSession("c", "r", "scout", data); es[0].Agent != "scout" {
		t.Fatalf("known agent overwritten: %+v", es)
	}
}

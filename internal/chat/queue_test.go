package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/store"
)

func TestComposeMessage(t *testing.T) {
	cases := []struct {
		in   []store.QueueEntry
		want string
	}{
		{[]store.QueueEntry{{Text: "one"}}, "one"},
		{[]store.QueueEntry{{Text: "one"}, {Text: " two "}}, "one\n\ntwo"},
		{[]store.QueueEntry{{Text: "a", Attachments: []string{"x.csv"}}}, "a\n\n[Attachments in /workspace/inputs/]\n- x.csv"},
		{[]store.QueueEntry{{Attachments: []string{"x.csv"}}}, "See attachments.\n\n[Attachments in /workspace/inputs/]\n- x.csv"},
		{[]store.QueueEntry{{Text: "a", Attachments: []string{"x.csv"}}, {Text: "b", Attachments: []string{"y.png", "x.csv"}}},
			"a\n\nb\n\n[Attachments in /workspace/inputs/]\n- x.csv\n- y.png"},
	}
	for _, c := range cases {
		if got := composeMessage(c.in, nil); got.Text != c.want || got.Origin != store.OriginUser {
			t.Errorf("composeMessage(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// prompts returns the messages that went to the agent.
func (a *fakeAgent) prompts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.cmds {
		if c["type"] == "prompt" {
			out = append(out, c["message"].(string))
		}
	}
	return out
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}

// busyChat creates a chat whose first run only ends when release is called.
func busyChat(t *testing.T, e *env) (id string, a *fakeAgent, release func()) {
	t.Helper()
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "q"})
	if err != nil {
		t.Fatal(err)
	}
	a = e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold = hold
	a.mu.Unlock()
	res, err := e.m.Send(ctx, c.ID, "one")
	if err != nil || res.Queued {
		t.Fatalf("first message: %+v %v", res, err)
	}
	return c.ID, a, func() { close(hold) }
}

func TestQueueWhileRunningDeliveredTogether(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, a, release := busyChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	if _, err := e.m.AddInput(ctx, id, "data.csv", []byte("x")); err != nil {
		t.Fatal(err)
	}
	r2, err := e.m.SendWithAttachments(ctx, id, "two", []string{"data.csv"})
	if err != nil || !r2.Queued || r2.QueueID == "" {
		t.Fatalf("second: %+v %v", r2, err)
	}
	r3, _ := e.m.Send(ctx, id, "three")
	r4, _ := e.m.Send(ctx, id, "four")
	if !r3.Queued || !r4.Queued {
		t.Fatalf("not enqueued: %+v %+v", r3, r4)
	}
	ev := waitEvent(t, events, "queue", "")
	if qe := ev.Data.(QueueEvent); qe.Change != "queued" || len(qe.Entries) != 1 {
		t.Fatalf("event: %+v", qe)
	}
	if err := e.m.Unqueue(ctx, id, r3.QueueID); err != nil {
		t.Fatal(err)
	}
	v, _ := e.m.View(ctx, id)
	if v.Queued != 2 || v.QueueHeld {
		t.Fatalf("during the run: queued=%d held=%v", v.Queued, v.QueueHeld)
	}
	if got := a.prompts(); len(got) != 1 {
		t.Fatalf("delivered before the end of the run: %q", got)
	}
	release()
	waitUntil(t, "delivery", func() bool { return len(a.prompts()) == 2 })
	want := "two\n\nfour\n\n[Attachments in /workspace/inputs/]\n- data.csv"
	if got := a.prompts()[1]; got != want {
		t.Fatalf("message:\n%q\nwant\n%q", got, want)
	}
	// The "delivered" event carries the text so that the UI can show the bubble without a gap.
	for {
		qe := waitEvent(t, events, "queue", "").Data.(QueueEvent)
		if qe.Change == "delivered" {
			if qe.Text != want || len(qe.IDs) != 2 || len(qe.Entries) != 0 {
				t.Fatalf("delivered: %+v", qe)
			}
			break
		}
	}
	if err := e.m.Unqueue(ctx, id, r2.QueueID); !errors.Is(err, ErrQueueDelivered) {
		t.Fatalf("removal after delivery: %v", err)
	}
	waitSettled(t, e, id)
	if q, _ := e.m.Queue(ctx, id); len(q) != 0 {
		t.Fatalf("queue not empty: %+v", q)
	}
}

func TestQueueHeldAfterAbort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, a, release := busyChat(t, e)
	if r, _ := e.m.Send(ctx, id, "two"); !r.Queued {
		t.Fatal("not enqueued")
	}
	if _, err := e.m.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}
	release()
	waitSettled(t, e, id)
	time.Sleep(100 * time.Millisecond) // background work after agent_settled
	if got := a.prompts(); len(got) != 1 {
		t.Fatalf("delivered after abort: %q", got)
	}
	v, _ := e.m.View(ctx, id)
	if v.Queued != 1 || !v.QueueHeld {
		t.Fatalf("after abort: queued=%d held=%v", v.Queued, v.QueueHeld)
	}
	// The next message takes the held one along.
	res, err := e.m.Send(ctx, id, "three")
	if err != nil || res.Queued {
		t.Fatalf("three: %+v %v", res, err)
	}
	if got := a.prompts(); len(got) != 2 || got[1] != "two\n\nthree" {
		t.Fatalf("messages: %q", got)
	}
	waitSettled(t, e, id)
	if v, _ := e.m.View(ctx, id); v.Queued != 0 || v.QueueHeld {
		t.Fatalf("afterwards: %+v", v)
	}
	// Without queued entries there is nothing to deliver.
	if _, err := e.m.FlushQueue(ctx, id); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty queue: %v", err)
	}
}

// The queue lives in Postgres and survives a restart of the orchestrator; afterwards the chat is
// idle, and "send now" resumes it with the queued messages.
func TestQueueSurvivesRestart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	if r, _ := e.m.Send(ctx, id, "after the restart"); !r.Queued {
		t.Fatal("not enqueued")
	}
	// Restart: new manager on the same database; the old chat is idle.
	e.m.Shutdown(ctx)
	release()
	m2 := NewManager(e.st, e.p, e.cat, e.blobs, artifacts.NewBroker(), e.opt)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := m2.View(ctx, id)
	if v.State != store.StateDormant || v.Queued != 1 || !v.QueueHeld {
		t.Fatalf("after restart: state=%s queued=%d held=%v", v.State, v.Queued, v.QueueHeld)
	}
	res, err := m2.FlushQueue(ctx, id)
	if err != nil || !res.Resumed {
		t.Fatalf("send now: %+v %v", res, err)
	}
	fresh := resumedAgent(e)
	if fresh == nil {
		t.Fatal("no fresh sandbox")
	}
	waitUntil(t, "message", func() bool {
		p := fresh.prompts()
		return len(p) == 1 && p[0] == "after the restart"
	})
	if q, _ := m2.Queue(ctx, id); len(q) != 0 {
		t.Fatalf("not delivered: %+v", q)
	}
}

// While a tool is running, a new message goes to pi right away via steer and is inserted after the
// tool in the same turn (like Claude Code), not only after the end of the turn.
func TestQueueSteeredWhileToolRuns(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "s"})
	a := e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold, a.withTool = hold, true
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "one"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "tool running", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		l := e.m.live[c.ID]
		return l != nil && l.toolsRunning == 1
	})
	r, err := e.m.Send(ctx, c.ID, "two")
	if err != nil || !r.Queued {
		t.Fatalf("second: %+v %v", r, err)
	}
	waitUntil(t, "steered in", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.steered) == 1 && a.steered[0] == "two" })
	if q, _ := e.m.Queue(ctx, c.ID); len(q) != 0 {
		t.Fatalf("still queued: %+v", q)
	}
	close(hold)
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	// one turn: one, two (inserted after the tool), then the answer
	if strings.Join(roles, ",") != "user,user,assistant" {
		t.Fatalf("history: %v", roles)
	}
	if n := strings.Count(strings.Join(a.commands(), ","), "prompt"); n != 2 {
		t.Fatalf("prompt calls: %d (%v)", n, a.commands())
	}
}

// If the user aborts before pi inserts the steered message, it is enqueued again (held) instead of
// getting lost.
func TestSteeredRestoredAfterAbort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "s"})
	a := e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold, a.withTool = hold, true
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "one"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "tool running", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		l := e.m.live[c.ID]
		return l != nil && l.toolsRunning == 1
	})
	if _, err := e.m.Send(ctx, c.ID, "two"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "steered in", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.steered) == 1 })
	if _, err := e.m.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	close(hold)
	waitSettled(t, e, c.ID)
	waitUntil(t, "enqueued again", func() bool { q, _ := e.m.Queue(ctx, c.ID); return len(q) == 1 && q[0].Text == "two" })
	if v, _ := e.m.View(ctx, c.ID); !v.QueueHeld {
		t.Fatalf("not held: %+v", v)
	}
}

// Pure orchestrator notes are not steered in: they go through deliverQueue at the end of the run
// (wake-up with its limits, Review 3, H2).
func TestSystemNoteNotSteered(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "s"})
	a := e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold, a.withTool = hold, true
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "one"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "tool running", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		l := e.m.live[c.ID]
		return l != nil && l.toolsRunning == 1
	})
	if _, err := e.st.EnqueueKind(ctx, c.ID, store.QueueSystem, "bg-1 is done", nil); err != nil {
		t.Fatal(err)
	}
	e.m.mu.Lock()
	l := e.m.live[c.ID]
	e.m.mu.Unlock()
	e.m.steerQueue(c.ID, l)
	a.mu.Lock()
	n := len(a.steered)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("note steered in: %d", n)
	}
	if q, _ := e.m.Queue(ctx, c.ID); len(q) != 1 {
		t.Fatalf("queue: %+v", q)
	}
	close(hold)
	waitSettled(t, e, c.ID)
}

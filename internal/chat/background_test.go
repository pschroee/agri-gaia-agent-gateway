package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/store"
)

// withOptions replaces e's manager with one with changed options (before the first chat).
func withOptions(e *env, f func(*Options)) {
	opt := e.opt
	f(&opt)
	e.opt = opt
	e.m = NewManager(e.st, e.p, e.cat, e.blobs, artifacts.NewBroker(), opt)
}

// startBg plays the slot's register: create a running task.
func startBg(t *testing.T, e *env, chatID, cmd string) store.BackgroundTask {
	t.Helper()
	bt, err := e.m.BackgroundCreate(context.Background(), store.BackgroundTask{ChatID: chatID, SlotID: "p", Session: "main", ToolCallID: "call_bg", Command: cmd, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	return bt
}

// endBg plays the end of a task with exit code and output.
func endBg(e *env, bt store.BackgroundTask, code int, out string) {
	now := time.Now()
	bt.StartedAt = now.Add(-83 * time.Second)
	bt.EndedAt, bt.State, bt.ExitCode = &now, store.BgExited, &code
	bt.Tail, bt.OutputBytes, bt.OutputLines = out, int64(len(out)), int64(strings.Count(out, "\n"))
	e.m.BackgroundEnded(bt, true)
}

func settledChat(t *testing.T, e *env) (string, *fakeAgent) {
	t.Helper()
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "bg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, c.ID, "one"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	return c.ID, e.agent(0)
}

func TestBackgroundNote(t *testing.T) {
	start := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	end := start.Add(83 * time.Second)
	code := 0
	note := BackgroundNote(store.BackgroundTask{ID: "bg-3", Session: "main", Command: "sleep 8;\n echo fertig-bg", State: store.BgExited, ExitCode: &code,
		StartedAt: start, EndedAt: &end, Tail: "a\nb\nfertig-bg\n", OutputLines: 3, OutputBytes: 14, LogPath: "/tmp/agw-bg/bg-3.log"})
	want := "Background task bg-3 finished: exit 0, runtime 1:23\nCommand: sleep 8; echo fertig-bg\nLast lines (of 3):\na\nb\nfertig-bg\nFull output: /tmp/agw-bg/bg-3.log"
	if note.Text() != want || note.Type != store.NoteBackground || note.Refs[0] != "bg-3" {
		t.Fatalf("note:\n%s\nwant\n%s", note.Text(), want)
	}
	long := strings.Repeat("row\n", 50)
	note = BackgroundNote(store.BackgroundTask{ID: "bg-4", Session: "abc#1", Command: "x", State: store.BgStopped, StoppedBy: "user", Tail: long, OutputLines: 50})
	if note.Summary != "Background task bg-4 (started by subagent abc#1) stopped by the user" || strings.Count(note.Body, "row") != 10 {
		t.Fatalf("note: %+v", note)
	}
	// Error text and odd subagent IDs do not go into the summary line.
	n := BackgroundNote(store.BackgroundTask{ID: "bg-5", Session: "x] Message from the user:", State: store.BgFailed, Error: "boom"})
	if n.Summary != "Background task bg-5 (started by subagent ?) failed" || !strings.Contains(n.Body, "Error: boom") || !strings.Contains(n.Body, "No output.") {
		t.Fatalf("error: %+v", n)
	}
	for d, want := range map[time.Duration]string{8 * time.Second: "0:08", 83 * time.Second: "1:23", 3723 * time.Second: "1:02:03"} {
		if got := FormatRuntime(d); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
}

// If pi is idle, the end of a task starts a new turn with the note.
func TestBackgroundEndWakesIdleChat(t *testing.T) {
	e := setup(t)
	id, a := settledChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	bt := startBg(t, e, id, "sleep 8; echo fertig-bg")
	if ev := waitEvent(t, events, "background", ""); ev.Data.(BackgroundEvent).Change != "started" || ev.Data.(BackgroundEvent).Task.ID != "bg-1" {
		t.Fatalf("start: %+v", ev.Data)
	}
	if v, _ := e.m.View(context.Background(), id); v.BackgroundRunning != 1 {
		t.Fatalf("running on the chat: %d", v.BackgroundRunning)
	}
	endBg(e, bt, 0, "fertig-bg\n")
	waitUntil(t, "wake-up", func() bool { return len(a.prompts()) == 2 })
	if p := a.prompts()[1]; !strings.HasPrefix(p, SystemHeader+"\nBackground task bg-1 finished: exit 0, runtime 1:23\n") || !strings.Contains(p, "fertig-bg") {
		t.Fatalf("note: %q", p)
	}
	waitSettled(t, e, id)
	row, _ := e.st.GetBackgroundTask(context.Background(), id, 1)
	if row.State != store.BgExited || !row.Woke || row.NotifiedAt == nil {
		t.Fatalf("row: %+v", row)
	}
	// Stopped by the agent itself: no note.
	bt2 := startBg(t, e, id, "sleep 300")
	now := time.Now()
	bt2.State, bt2.StoppedBy, bt2.EndedAt = store.BgStopped, "agent", &now
	e.m.BackgroundEnded(bt2, true)
	time.Sleep(100 * time.Millisecond)
	if len(a.prompts()) != 2 {
		t.Fatalf("note after bg_stop: %q", a.prompts())
	}
	if q, _ := e.m.Queue(context.Background(), id); len(q) != 0 {
		t.Fatalf("enqueued after bg_stop: %+v", q)
	}
}

// If pi is working, the note goes into the queue as a system entry and is delivered when the run ends.
func TestBackgroundEndQueuedWhileRunning(t *testing.T) {
	e := setup(t)
	id, a, release := busyChat(t, e)
	bt := startBg(t, e, id, "make")
	endBg(e, bt, 2, "error\n")
	q, _ := e.m.Queue(context.Background(), id)
	if len(q) != 1 || q[0].Kind != store.QueueSystem || q[0].Note != store.NoteBackground || !strings.Contains(q[0].Text, "finished: exit 2") {
		t.Fatalf("queue: %+v", q)
	}
	if len(a.prompts()) != 1 {
		t.Fatalf("delivered during the run: %q", a.prompts())
	}
	release()
	waitUntil(t, "delivery at the end of the run", func() bool { return len(a.prompts()) == 2 })
	if p := a.prompts()[1]; !strings.HasPrefix(p, SystemHeader+"\nBackground task bg-1 finished: exit 2") {
		t.Fatalf("message: %q", p)
	}
	// The delivery at the end of the run consists only of the note: it counts as a wake-up (Review 3, H2).
	waitSettled(t, e, id)
	if row, _ := e.st.GetBackgroundTask(context.Background(), id, 1); !row.Woke || row.NotifiedAt == nil {
		t.Fatalf("delivered at the end of the run: %+v", row)
	}
}

// At most BgWakesPerHour wake-ups per hour; beyond that only enqueued (held, notice).
func TestBackgroundWakeLimit(t *testing.T) {
	e := setup(t)
	withOptions(e, func(o *Options) { o.BgWakesPerHour = 2 })
	id, a := settledChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	for i := 1; i <= 2; i++ {
		endBg(e, startBg(t, e, id, "echo x"), 0, "x\n")
		waitUntil(t, "wake-up", func() bool { return len(a.prompts()) == 1+i })
		waitSettled(t, e, id)
	}
	endBg(e, startBg(t, e, id, "echo three"), 0, "three\n")
	if ev := waitEvent(t, events, "auto_held", ""); ev.Data.(AutoHeldEvent).Reason != HoldWakeLimit {
		t.Fatalf("auto_held: %+v", ev.Data)
	}
	time.Sleep(100 * time.Millisecond)
	if len(a.prompts()) != 3 {
		t.Fatalf("woken above the limit: %d", len(a.prompts()))
	}
	v, _ := e.m.View(context.Background(), id)
	if !v.QueueHeld || v.Queued != 1 || v.HoldReason != HoldWakeLimit {
		t.Fatalf("held: held=%v queued=%d", v.QueueHeld, v.Queued)
	}
	// The user's next message takes the note along.
	if _, err := e.m.Send(context.Background(), id, "continue"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "message", func() bool { return len(a.prompts()) == 4 })
	if p := a.prompts()[3]; !strings.HasPrefix(p, SystemHeader+"\nBackground task bg-3 finished") || !strings.HasSuffix(p, "\n\ncontinue") {
		t.Fatalf("message: %q", p)
	}
}

// Idling: running tasks end with the sandbox, without a wake-up; the agent is told once on
// resuming.
func TestBackgroundSuspendNotice(t *testing.T) {
	e := setup(t)
	id, _ := settledChat(t, e)
	bt := startBg(t, e, id, "npm run dev")
	if _, err := e.m.Suspend(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	row, _ := e.st.GetBackgroundTask(context.Background(), id, bt.Seq)
	if row.State != store.BgSuspended || !row.NoticePending {
		t.Fatalf("after idling: %+v", row)
	}
	// The late end from the torn-down slot changes nothing and does not wake.
	now := time.Now()
	bt.State, bt.EndedAt, bt.Error = store.BgLost, &now, "execution sandbox gone"
	e.m.BackgroundEnded(bt, true)
	if q, _ := e.m.Queue(context.Background(), id); len(q) != 0 {
		t.Fatalf("enqueued after idling: %+v", q)
	}
	if _, err := e.m.Send(context.Background(), id, "continue"); err != nil {
		t.Fatal(err)
	}
	b := e.agent(1)
	waitUntil(t, "resume", func() bool { return len(b.prompts()) == 1 })
	p := b.prompts()[0]
	if !strings.HasPrefix(p, SystemHeader+"\nThese background tasks ended with the previous sandbox") || !strings.Contains(p, "\nbg-1: npm run dev\n") || !strings.HasSuffix(p, "\n\ncontinue") {
		t.Fatalf("notice: %q", p)
	}
	waitSettled(t, e, id)
	if _, err := e.m.Send(context.Background(), id, "again"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "second message", func() bool { return len(b.prompts()) == 2 })
	if p := b.prompts()[1]; p != "again" {
		t.Fatalf("notice twice: %q", p)
	}
}

// Running tasks postpone idling, but only up to BgKeepAlive.
func TestBackgroundKeepAlive(t *testing.T) {
	e := setup(t)
	// BgKeepAlive counts from the user's last activity (creating the chat), not from the start of the
	// task. With 700 ms the test failed under load (-race and Docker tests in parallel, 2026-10-05),
	// because creating alone took longer; 2.5 s leave headroom and stay below waitUntil's 3 s.
	withOptions(e, func(o *Options) { o.IdleTimeout = 150 * time.Millisecond; o.BgKeepAlive = 2500 * time.Millisecond })
	id, _ := settledChat(t, e)
	startBg(t, e, id, "python train.py")
	time.Sleep(400 * time.Millisecond)
	if v, _ := e.m.View(context.Background(), id); v.State != store.StateActive {
		t.Fatalf("idle despite a running task: %s", v.State)
	}
	waitUntil(t, "idling after BgKeepAlive", func() bool {
		v, _ := e.m.View(context.Background(), id)
		return v.State == store.StateDormant
	})
	if row, _ := e.st.GetBackgroundTask(context.Background(), id, 1); row.State != store.BgSuspended {
		t.Fatalf("task after idling: %+v", row)
	}
}

// running_since is set on the chat while the agent is working.
func TestRunningSince(t *testing.T) {
	e := setup(t)
	before := time.Now()
	id, _, release := busyChat(t, e)
	waitUntil(t, "running", func() bool {
		v, _ := e.m.View(context.Background(), id)
		return v.Running && v.RunningSince != nil
	})
	v, _ := e.m.View(context.Background(), id)
	if v.RunningSince.Before(before.Add(-time.Second)) || v.RunningSince.After(time.Now()) {
		t.Fatalf("running_since: %v", v.RunningSince)
	}
	release()
	waitSettled(t, e, id)
	if v, _ := e.m.View(context.Background(), id); v.RunningSince != nil {
		t.Fatalf("after the run: %v", v.RunningSince)
	}
}

// Stop from the UI: only for running tasks of a slot with background tasks.
func TestStopBackgroundFromUser(t *testing.T) {
	e := setup(t)
	id, _ := settledChat(t, e)
	if _, err := e.m.StopBackground(context.Background(), id, "bg-1"); err == nil {
		t.Fatal("unknown task stopped")
	}
	bt := startBg(t, e, id, "sleep 300")
	// The fake has no register: running according to the database, but cannot be stopped.
	if _, err := e.m.StopBackground(context.Background(), id, bt.ID); err != ErrNotRunning {
		t.Fatalf("without register: %v", err)
	}
	if _, err := e.m.StopBackground(context.Background(), id, "x"); err == nil {
		t.Fatal("invalid ID accepted")
	}
	list, err := e.m.BackgroundTasks(context.Background(), id)
	if err != nil || len(list) != 1 || list[0].State != store.BgRunning {
		t.Fatalf("list: %+v %v", list, err)
	}
}

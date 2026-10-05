package store

import (
	"context"
	"sync"
	"testing"
)

func TestBackgroundTaskLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "bg", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	logPath := func(n int) string { return "/tmp/agw-bg/bg-" + BgID(n)[3:] + ".log" }
	// Numbers per chat are consecutive, even when created concurrently.
	var wg sync.WaitGroup
	seqs := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bt, err := s.CreateBackgroundTask(ctx, BackgroundTask{ChatID: c.ID, SlotID: "p-1", ToolCallID: "call_x", Command: "sleep 1\x00"}, logPath)
			if err != nil {
				t.Error(err)
				return
			}
			seqs <- bt.Seq
		}()
	}
	wg.Wait()
	close(seqs)
	seen := map[int]bool{}
	for n := range seqs {
		seen[n] = true
	}
	if len(seen) != 4 || !seen[1] || !seen[4] {
		t.Fatalf("numbers: %v", seen)
	}
	got, err := s.GetBackgroundTask(ctx, c.ID, 2)
	if err != nil || got.ID != "bg-2" || got.State != BgRunning || got.LogPath != "/tmp/agw-bg/bg-2.log" || got.Command != "sleep 1␀" || got.Session != "main" {
		t.Fatalf("task 2: %+v %v", got, err)
	}
	if ch, _ := s.GetChat(ctx, c.ID); ch.BackgroundRunning != 4 {
		t.Fatalf("running on the chat: %d", ch.BackgroundRunning)
	}
	// Record the end: effective once, afterwards only add output.
	code := 0
	fin := BackgroundTask{ChatID: c.ID, Seq: 1, State: BgExited, ExitCode: &code, OutputBytes: 9, OutputLines: 1, OutputExcerpt: "done-bg\n", OutputSHA256: "abc", Tail: "done-bg\n"}
	if ok, err := s.FinishBackgroundTask(ctx, fin); !ok || err != nil {
		t.Fatalf("end: %v %v", ok, err)
	}
	fin.State = BgFailed
	if ok, _ := s.FinishBackgroundTask(ctx, fin); ok {
		t.Fatal("second end overwrites the state")
	}
	if got, _ := s.GetBackgroundTask(ctx, c.ID, 1); got.State != BgExited || *got.ExitCode != 0 || got.Tail != "done-bg\n" || got.EndedAt == nil {
		t.Fatalf("after the end: %+v", got)
	}
	// Record note and wake-up (wake-ups are counted in chat_turns, see turns_test.go).
	if err := s.MarkBackgroundNotified(ctx, c.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBackgroundWoke(ctx, c.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackgroundTask(ctx, c.ID, 1); got.NotifiedAt == nil || !got.Woke {
		t.Fatalf("notified and woken: %+v", got)
	}
	// Idling: the remaining three end with a notice; a late end from the sandbox changes nothing anymore,
	// but adds the output.
	ended, err := s.EndRunningBackground(ctx, c.ID, BgSuspended, "ended while idling", true)
	if err != nil || len(ended) != 3 {
		t.Fatalf("idling: %d %v", len(ended), err)
	}
	late := BackgroundTask{ChatID: c.ID, Seq: 2, State: BgLost, OutputBytes: 3, OutputSHA256: "def", Tail: "ab\n"}
	if ok, _ := s.FinishBackgroundTask(ctx, late); ok {
		t.Fatal("late end overwrites suspended")
	}
	if got, _ := s.GetBackgroundTask(ctx, c.ID, 2); got.State != BgSuspended || got.OutputSHA256 != "def" || !got.NoticePending {
		t.Fatalf("after late end: %+v", got)
	}
	notes, _ := s.BackgroundNotices(ctx, c.ID)
	if len(notes) != 3 {
		t.Fatalf("notices: %d", len(notes))
	}
	if err := s.ClearBackgroundNotices(ctx, c.ID, []int{2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if notes, _ := s.BackgroundNotices(ctx, c.ID); len(notes) != 0 {
		t.Fatalf("notices after clearing: %d", len(notes))
	}
	if ch, _ := s.GetChat(ctx, c.ID); ch.BackgroundRunning != 0 {
		t.Fatalf("running after idling: %d", ch.BackgroundRunning)
	}
	// Restart of the orchestrator
	bt, _ := s.CreateBackgroundTask(ctx, BackgroundTask{ChatID: c.ID, SlotID: "p-2", ToolCallID: "call_y", Command: "x"}, nil)
	if bt.Seq != 5 {
		t.Fatalf("next number: %d", bt.Seq)
	}
	if n, err := s.EndAllRunningBackground(ctx, "orchestrator restarted"); err != nil || n != 1 {
		t.Fatalf("restart: %d %v", n, err)
	}
	list, _ := s.ListBackgroundTasks(ctx, c.ID)
	if len(list) != 5 || list[4].State != BgLost {
		t.Fatalf("list: %+v", list)
	}
	if ParseBgID("bg-12") != 12 || ParseBgID("bg-0") != 0 || ParseBgID("12") != 0 || ParseBgID("bg-1x") != 0 || ParseBgID("bg-01") != 0 {
		t.Fatal("ParseBgID")
	}
}

func TestQueueSystemEntry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "q", Model: "m", Variant: "cli"})
	e, err := s.EnqueueKind(ctx, c.ID, QueueSystem, "[Background task bg-1 ended]", nil)
	if err != nil || e.Kind != QueueSystem {
		t.Fatalf("%+v %v", e, err)
	}
	u, _ := s.Enqueue(ctx, c.ID, "hello", nil)
	if u.Kind != QueueUser {
		t.Fatalf("kind: %q", u.Kind)
	}
	list, _ := s.ClaimQueue(ctx, c.ID)
	if len(list) != 2 || list[0].Kind != QueueSystem || list[1].Kind != QueueUser {
		t.Fatalf("%+v", list)
	}
}

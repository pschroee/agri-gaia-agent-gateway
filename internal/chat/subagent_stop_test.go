// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agw/internal/sock"
	"agw/internal/store"
)

// relayRefOf reads the ref of the relay command the gateway put into the prompt.
func relayRefOf(msg string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(msg, relayCommand+" "))
	if err != nil {
		return ""
	}
	var req struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(raw, &req)
	return req.Ref
}

// stopEnv prepares a chat with one running subagent whose status row is stored, and lets the fake pi answer the
// relay with res. StopSubagent waits for that answer, so the hook has to reply while the test is inside the call.
func stopEnv(t *testing.T, res sock.SubagentRelayResult) (*env, *fakeAgent, string, func()) {
	t.Helper()
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	waitRunning(t, e, id)
	if _, err := e.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: id, RunID: "run-1", Agent: "worker", PiRunID: "pi-1", State: "running"}); err != nil {
		t.Fatal(err)
	}
	l := e.m.liveOf(id)
	if l == nil {
		t.Fatal("chat is not live")
	}
	l.pollMu.Lock()
	l.track("run-1", time.Now()).State = "running"
	l.pollMu.Unlock()
	a := e.agent(0)
	a.mu.Lock()
	a.onPrompt = func(msg string) {
		r := res
		r.Ref = relayRefOf(msg)
		if err := e.m.SubagentRelayResult(context.Background(), id, r); err != nil {
			t.Errorf("relay result: %v", err)
		}
	}
	a.mu.Unlock()
	return e, a, id, release
}

// The stop goes over the same relay as a message to a subagent, with the method stop and nothing but the run's ID
// (issue #120, L3, DL3.1).
func TestStopSubagentRelaysStop(t *testing.T) {
	e, a, id, release := stopEnv(t, sock.SubagentRelayResult{OK: true})
	defer release()
	run, err := e.m.StopSubagent(context.Background(), id, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-1" || run.StoppedBy != StoppedByUser {
		t.Fatalf("stopped run: %+v", run)
	}
	cmds := relayCmdsOf(a)
	if len(cmds) != 1 {
		t.Fatalf("relay commands: %d", len(cmds))
	}
	req := cmds[0]
	if req["method"] != "stop" || req["ref"] != "" && req["ref"] == nil {
		t.Fatalf("relay request: %+v", req)
	}
	params, _ := req["params"].(map[string]any)
	if len(params) != 1 || params["id"] != "pi-1" {
		t.Fatalf("params of the stop: %+v", params)
	}
}

// A stop records who stopped it and tells the main agent: the run carries stopped_by, the call log the subagent_stop
// entry and the queue a passive note that never wakes the agent (issue #120, L3, DL3.2, DL3.5).
func TestStopSubagentSetsStoppedByAndNotes(t *testing.T) {
	e, _, id, release := stopEnv(t, sock.SubagentRelayResult{OK: true})
	defer release()
	ctx := context.Background()
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	if _, err := e.m.StopSubagent(ctx, id, "run-1"); err != nil {
		t.Fatal(err)
	}
	rec, err := e.st.SubagentRunOf(ctx, id, "run-1")
	if err != nil || rec.StoppedBy != StoppedByUser {
		t.Fatalf("stopped_by: %+v %v", rec, err)
	}
	// the run goes out over SSE so the UI leaves "stopping …" without a reload
	ev := waitEvent(t, events, "subagent_run", "")
	if r, ok := ev.Data.(store.SubagentRun); !ok || r.RunID != "run-1" || r.StoppedBy != StoppedByUser {
		t.Fatalf("published run: %+v", ev.Data)
	}
	calls, err := e.st.ListSocketCalls(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range calls {
		if c.Op == opSubagentStop {
			found = true
			if c.Via != "ui" || c.Detail != "run-1" || c.Result != "ok" {
				t.Fatalf("socket call: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("no %s in the call log: %+v", opSubagentStop, calls)
	}
	queue, err := e.st.ListQueue(ctx, id)
	if err != nil || len(queue) != 1 || queue[0].Note != store.NoteSubagentStop {
		t.Fatalf("passive note: %+v %v", queue, err)
	}
	if !onlyPassive(queue) {
		t.Fatal("the note about a stop would wake the agent")
	}
	if txt := queue[0].Text; !strings.Contains(txt, "The user stopped subagent worker (run-1)") || !strings.Contains(txt, "do not restart it") {
		t.Fatalf("text of the note: %q", txt)
	}
}

// A run that is not running any more cannot be stopped; a chat that rests takes no stop at all (issue #120, L3).
func TestStopSubagentNotRunning(t *testing.T) {
	e, _, id, release := stopEnv(t, sock.SubagentRelayResult{OK: true})
	defer release()
	ctx := context.Background()
	l := e.m.liveOf(id)
	l.pollMu.Lock()
	l.subRuns["run-1"].State = "complete"
	l.pollMu.Unlock()
	if _, err := e.m.StopSubagent(ctx, id, "run-1"); !errors.Is(err, ErrSubagentNotRunning) {
		t.Fatalf("finished run: %v", err)
	}
}

// A chat without a sandbox has nothing to stop, and a stop must not wake it (issue #120, L3, DL3.2).
func TestStopSubagentDormant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := settledChat(t, e)
	if _, err := e.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: id, RunID: "run-1", Agent: "worker", PiRunID: "pi-1", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Suspend(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.StopSubagent(ctx, id, "run-1"); !errors.Is(err, ErrSubagentsUnavailable) {
		t.Fatalf("dormant chat: %v", err)
	}
	if v, _ := e.m.View(ctx, id); v.State != store.StateDormant {
		t.Fatalf("the chat woke up: %+v", v.State)
	}
}

// A chat in the trash takes no stop (issue #120, L3, DL3.2).
func TestStopSubagentInTrash(t *testing.T) {
	e, _, id, release := stopEnv(t, sock.SubagentRelayResult{OK: true})
	defer release()
	ctx := context.Background()
	if _, err := e.m.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.StopSubagent(ctx, id, "run-1"); !errors.Is(err, ErrTrashed) {
		t.Fatalf("chat in the trash: %v", err)
	}
}

// A refusal of pi-subagents travels with its code (issue #120, L3).
func TestStopSubagentRelayRefusal(t *testing.T) {
	e, _, id, release := stopEnv(t, sock.SubagentRelayResult{OK: false, Code: "invalid_state", Message: "run is cancelled"})
	defer release()
	ctx := context.Background()
	_, err := e.m.StopSubagent(ctx, id, "run-1")
	var relay *SubagentRelayError
	if !errors.As(err, &relay) || relay.Code != "invalid_state" {
		t.Fatalf("refusal: %v", err)
	}
	// nothing was recorded: the run stays without stopped_by
	if rec, _ := e.st.SubagentRunOf(ctx, id, "run-1"); rec.StoppedBy != "" {
		t.Fatalf("stopped_by after a refusal: %+v", rec)
	}
}

// A stop has no stored message, so its ref is vouched for by the open relays alone; everything else stays refused
// (issue #120, L3, DL3.3).
func TestRelayResultAcceptsStopRef(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.st.CreateChat(ctx, store.NewChat{Title: "stop", Model: "p/m", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan relayReply, 1)
	e.m.mu.Lock()
	e.m.stopRefs = map[string]string{"agw-0a1b2c3d": c.ID}
	e.m.relayWait = map[string]chan relayReply{"agw-0a1b2c3d": ch}
	e.m.mu.Unlock()
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: "agw-0a1b2c3d", OK: true}); err != nil {
		t.Fatalf("reply to an open stop: %v", err)
	}
	if r := <-ch; !r.OK {
		t.Fatalf("reply handed over: %+v", r)
	}
	// the same ref again: gone
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: "agw-0a1b2c3d", OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second reply: %v", err)
	}
	// a stop ref of another chat does not vouch for this chat
	e.m.mu.Lock()
	e.m.stopRefs = map[string]string{"agw-11111111": "00000000-0000-0000-0000-000000000001"}
	e.m.relayWait = map[string]chan relayReply{"agw-11111111": make(chan relayReply, 1)}
	e.m.mu.Unlock()
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: "agw-11111111", OK: true}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign stop ref: %v", err)
	}
}

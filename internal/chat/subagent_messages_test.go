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

// relayCmdsOf returns the relay commands the fake pi received, decoded: method, ref and params.
func relayCmdsOf(a *fakeAgent) []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, cmd := range a.cmds {
		msg, _ := cmd["message"].(string)
		if cmd["type"] != "prompt" || !strings.HasPrefix(msg, relayCommand+" ") {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(msg, relayCommand+" "))
		if err != nil {
			continue
		}
		var req map[string]any
		if json.Unmarshal(raw, &req) == nil {
			out = append(out, req)
		}
	}
	return out
}

// liveOf returns the live state of a chat in the test (nil when the chat rests).
func (m *Manager) liveOf(chatID string) *live {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[chatID]
}

// startSeeded wakes a seeded chat with a message, so that it has a sandbox.
func startSeeded(t *testing.T, e *env, id string) {
	t.Helper()
	if _, err := e.m.Send(context.Background(), id, "Hallo"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, id)
}

// seedRun stores a run of a chat with the given state and returns the chat.
func seedRun(t *testing.T, e *env, owner, variant, run, agent, piRun, state string) store.Chat {
	t.Helper()
	ctx := context.Background()
	c, err := e.st.CreateChat(ctx, store.NewChat{Title: "Subagent chat", Model: "deepseek/deepseek-flash", Variant: variant, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if run != "" {
		if _, err := e.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: c.ID, RunID: run, Agent: agent, PiRunID: piRun, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// Refusals that need no sandbox: text, run, agent, binding, trash and the gateway's own commands.
func TestSubagentMessageRefusals(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "done")
	if _, err := e.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: c.ID, RunID: "run-2", Agent: "claude-code", State: "done"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "   ", strings.Repeat("x", maxSubagentMessage+1)} {
		if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1", text); !errors.Is(err, ErrInvalid) {
			t.Fatalf("text of %d bytes: %v", len(text), err)
		}
	}
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run 1", "hallo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe run key: %v", err)
	}
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-404", "hallo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run of no chat: %v", err)
	}
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-2", "hallo"); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("agent not allowed: %v", err)
	}
	if _, err := e.m.SendMessage(ctx, c.ID, "/agw-subagent-relay abc", nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("user sends a gateway command: %v", err)
	}
	if _, err := e.m.RunCommand(ctx, c.ID, "/agw-subagent-relay abc"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RunCommand passes a gateway command: %v", err)
	}
	api := seedRun(t, e, "alice", "api", "run-1", "worker", "pi-1", "done")
	if _, err := e.m.SendSubagentMessage(ctx, api.ID, "run-1", "hallo"); !errors.Is(err, ErrSubagentsUnavailable) {
		t.Fatalf("binding without cli: %v", err)
	}
	if _, err := e.m.Trash(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1", "hallo"); !errors.Is(err, ErrTrashed) {
		t.Fatalf("chat in the trash: %v", err)
	}
}

// One message per run at a time, and 20 per chat and hour.
func TestSubagentMessagePendingAndRate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "done")
	if _, err := e.st.CreateSubagentMessage(ctx, store.SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-00000001", Mode: "resume",
		Text: "first", Status: store.SubagentMsgSent}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1", "second"); !errors.Is(err, ErrMessagePending) {
		t.Fatalf("second message while the first is on its way: %v", err)
	}
	if _, err := e.st.SetSubagentMessageStatus(ctx, c.ID, "agw-00000001", store.SubagentMsgDelivered, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSubagentMessagesPerHr; i++ {
		if _, err := e.st.CreateSubagentMessage(ctx, store.SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: newSubagentRef(), Mode: "resume",
			Text: "old", Status: store.SubagentMsgDelivered}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1", "one too many"); !errors.Is(err, ErrMessageRate) {
		t.Fatalf("rate limit: %v", err)
	}
}

// A run that is running inside a tool call of the main agent (no own id at pi-subagents) cannot be written to.
func TestSubagentMessageForegroundRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-fg", "worker", "", "running")
	startSeeded(t, e, c.ID)
	l := e.m.liveOf(c.ID)
	if l == nil {
		t.Fatal("chat is not live")
	}
	l.pollMu.Lock()
	l.track("run-fg", time.Now()).State = "running"
	l.pollMu.Unlock()
	if _, err := e.m.SendSubagentMessage(ctx, c.ID, "run-fg", "hint"); !errors.Is(err, ErrSubagentForeground) {
		t.Fatalf("foreground run: %v", err)
	}
}

// A running subagent gets the message as a steer; the relay carries the fixed parameters and the message in its
// own envelope with the marker; the gateway waits for the reply and records it. The main agent gets a passive note.
func TestSubagentMessageSteersRunningRun(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1#2", "worker", "pi-1", "running")
	startSeeded(t, e, c.ID)
	l := e.m.liveOf(c.ID)
	l.pollMu.Lock()
	l.track("run-1#2", time.Now()).State = "running"
	l.pollMu.Unlock()
	msg, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1#2", "Nimm statt 30 Sekunden nur 10")
	if err != nil || msg.Mode != "steer" || msg.Status != store.SubagentMsgSending {
		t.Fatalf("SendSubagentMessage: %+v %v", msg, err)
	}
	a := e.agent(0)
	waitFor(t, func() bool { return len(relayCmdsOf(a)) == 1 })
	req := relayCmdsOf(a)[0]
	params, _ := req["params"].(map[string]any)
	if req["method"] != "steer" || req["ref"] != msg.Ref || params["id"] != "pi-1" || params["index"] != float64(2) || params["mode"] != "steer" {
		t.Fatalf("relay request: %+v", req)
	}
	if !strings.Contains(params["message"].(string), "[Message from the user, sent directly to you · ref "+msg.Ref+"]") {
		t.Fatalf("message without marker: %q", params["message"])
	}
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: msg.Ref, OK: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		got, err := e.st.SubagentMessageByRef(ctx, c.ID, msg.Ref)
		return err == nil && got.Status == store.SubagentMsgSent
	})
	queue, err := e.st.ListQueue(ctx, c.ID)
	if err != nil || len(queue) != 1 || queue[0].Note != store.NoteSubagentUser || onlyPassive(queue) == false {
		t.Fatalf("passive note: %+v %v", queue, err)
	}
	// the reply of a second relay for the same ref is refused, nothing changes
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: msg.Ref, OK: false, Message: "late"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second reply: %v", err)
	}
}

// A finished run with an own id is resumed; when pi-subagents cannot resume it, a new run of the same agent starts
// with the transcript the gateway recorded (mode restart), inside a fence with a fresh marker.
func TestSubagentMessageFallsBackToRestart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "complete")
	startSeeded(t, e, c.ID)
	if _, err := e.st.AddSubagentEntries(ctx, []store.SubagentEntry{{ChatID: c.ID, RunID: "run-1", EntryID: "t1", Agent: "worker", Kind: "task",
		Payload: json.RawMessage(`{"text":"Warte 30 Sekunden"}`)}}); err != nil {
		t.Fatal(err)
	}
	msg, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1", "Wie lange hat es genau gedauert?")
	if err != nil || msg.Mode != "resume" {
		t.Fatalf("SendSubagentMessage: %+v %v", msg, err)
	}
	a := e.agent(0)
	waitFor(t, func() bool { return len(relayCmdsOf(a)) >= 1 })
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: msg.Ref, OK: false, Code: "not_found", Message: "Async run 'pi-1' was not found in the active session."}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(relayCmdsOf(a)) >= 2 })
	spawn := relayCmdsOf(a)[1]
	params, _ := spawn["params"].(map[string]any)
	task, _ := params["task"].(string)
	if spawn["method"] != "spawn" || params["agent"] != "worker" || params["async"] != true {
		t.Fatalf("spawn request: %+v", spawn)
	}
	if !strings.HasPrefix(task, restartHeader) || !strings.Contains(task, "Task: Warte 30 Sekunden") || !strings.Contains(task, "<<<") {
		t.Fatalf("restart task: %q", task)
	}
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: msg.Ref, OK: true, RunID: "run-7"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		got, err := e.st.SubagentMessageByRef(ctx, c.ID, msg.Ref)
		return err == nil && got.Status == store.SubagentMsgSent && got.Mode == "restart" && got.TargetRunID == ""
	})
}

// A chat that rests has no running subagents: its sandbox is started on the message, and the run continues with the
// gateway's transcript.
func TestSubagentMessageDormantChatResumes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "researcher", "pi-1", "complete")
	msg, err := e.m.SendSubagentMessage(ctx, c.ID, "run-1", "Folgefrage")
	if err != nil || msg.Mode != "restart" {
		t.Fatalf("dormant chat: %+v %v", msg, err)
	}
	waitFor(t, func() bool { return e.m.liveOf(c.ID) != nil })
	a := e.agent(0)
	waitFor(t, func() bool { return len(relayCmdsOf(a)) == 1 })
	if relayCmdsOf(a)[0]["method"] != "spawn" {
		t.Fatalf("dormant chat relays %+v", relayCmdsOf(a)[0])
	}
}

// A message to a subagent starts no turn of the main agent and wakes nothing: no turn record, the passive note only.
func TestPassiveNoteNeverWakes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "", "complete")
	e.m.notePassive(ctx, c.ID, store.SubagentRun{RunID: "run-1", Agent: "worker"}, "resume", "Wie lange?")
	queue, err := e.st.ListQueue(ctx, c.ID)
	if err != nil || len(queue) != 1 || !onlyPassive(queue) {
		t.Fatalf("queue after the note: %+v %v", queue, err)
	}
	if !strings.Contains(queue[0].Text, "The user wrote directly to subagent run-1 (worker); it continues with its history in a new run.") {
		t.Fatalf("summary: %q", queue[0].Text)
	}
	if !strings.Contains(queue[0].Text, "Wie lange?") {
		t.Fatalf("text of the user missing: %q", queue[0].Text)
	}
	turns, err := e.st.Turns(ctx, c.ID)
	if err != nil || len(turns) != 0 {
		t.Fatalf("a note started a turn: %+v %v", turns, err)
	}
}

// The echo of a message in a subagent's session marks it delivered and links a new run to the addressed one.
func TestEchoMarksDeliveredAndLinksContinuation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "complete")
	if _, err := e.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: c.ID, RunID: "run-2", Agent: "worker", State: "running"}); err != nil {
		t.Fatal(err)
	}
	msg, err := e.st.CreateSubagentMessage(ctx, store.SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-0a1b2c3d", Mode: "resume",
		Text: "Folgefrage", Status: store.SubagentMsgSent})
	if err != nil {
		t.Fatal(err)
	}
	e.m.echoArrived(ctx, c.ID, "run-2", msg.Ref)
	got, err := e.st.SubagentMessageByRef(ctx, c.ID, msg.Ref)
	if err != nil || got.Status != store.SubagentMsgDelivered || got.TargetRunID != "run-2" {
		t.Fatalf("delivered message: %+v %v", got, err)
	}
	r, err := e.st.SubagentRunOf(ctx, c.ID, "run-2")
	if err != nil || r.ContinuesRunID != "run-1" {
		t.Fatalf("continuation: %+v %v", r, err)
	}
}

// A steer whose run ends before the echo arrives has failed.
func TestSteerFailsWhenRunEndsWithoutEcho(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "running")
	msg, err := e.st.CreateSubagentMessage(ctx, store.SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-0a1b2c3d", Mode: "steer",
		Text: "zu spät", Status: store.SubagentMsgSent})
	if err != nil {
		t.Fatal(err)
	}
	e.m.failSteers(ctx, c.ID, "run-1")
	got, err := e.st.SubagentMessageByRef(ctx, c.ID, msg.Ref)
	if err != nil || got.Status != store.SubagentMsgFailed || got.Error != "run ended before the message arrived" {
		t.Fatalf("steer after the end: %+v %v", got, err)
	}
}

// A relay reply that never comes ends in a failed message (the timeout of the relay).
func TestRelayTimeoutIsReported(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "complete")
	msg, err := e.st.CreateSubagentMessage(ctx, store.SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-0a1b2c3d", Mode: "resume",
		Text: "m", Status: store.SubagentMsgSending})
	if err != nil {
		t.Fatal(err)
	}
	e.m.settleSubagentMessage(ctx, c.ID, msg.Ref, store.SubagentMsgFailed, errRelayTimeout.Error(), "", "")
	got, _ := e.st.SubagentMessageByRef(ctx, c.ID, msg.Ref)
	if got.Status != store.SubagentMsgFailed || got.Error != "relay timed out" {
		t.Fatalf("timeout: %+v", got)
	}
}

// A late "sent" never downgrades a delivered message.
func TestSettleKeepsDelivered(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "running")
	msg, err := e.st.CreateSubagentMessage(ctx, store.SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-0a1b2c3d", Mode: "steer",
		Text: "m", Status: store.SubagentMsgDelivered})
	if err != nil {
		t.Fatal(err)
	}
	e.m.settleSubagentMessage(ctx, c.ID, msg.Ref, store.SubagentMsgSent, "", "", "")
	got, _ := e.st.SubagentMessageByRef(ctx, c.ID, msg.Ref)
	if got.Status != store.SubagentMsgDelivered {
		t.Fatalf("delivered downgraded to %s", got.Status)
	}
}

// The reply of an unknown or foreign ref is ErrNotFound and changes nothing.
func TestRelayResultUnknownRef(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := seedRun(t, e, "alice", "cli", "run-1", "worker", "pi-1", "complete")
	if err := e.m.SubagentRelayResult(ctx, c.ID, sock.SubagentRelayResult{Ref: "agw-deadbeef", OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref: %v", err)
	}
}

// Pure parts: the marker, the echo in a session file, the transcript lines and the restart decision.
func TestRelayMarkerAndEchoParsing(t *testing.T) {
	ref := "agw-0a1b2c3d"
	text := userBlock(ref, "Nimm 10")
	if !strings.HasPrefix(text, "[Message from the user, sent directly to you · ref "+ref+"]\n\nNimm 10") {
		t.Fatalf("envelope: %q", text)
	}
	line, _ := json.Marshal(map[string]any{"type": "message", "id": "e1",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}}})
	known := func(r string) bool { return r == ref }
	got := parseChildSessionKnown("chat", "run-1", "worker", string(line), known)
	if len(got) != 1 || got[0].Kind != "user_echo" || !strings.Contains(string(got[0].Payload), `"relay_ref":"`+ref+`"`) {
		t.Fatalf("echo: %+v", got)
	}
	other := parseChildSessionKnown("chat", "run-1", "worker", string(line), func(string) bool { return false })
	if len(other) != 1 || other[0].Kind != "task" {
		t.Fatalf("unknown marker must stay a task: %+v", other)
	}
	if got := parseChildSession("chat", "run-1", "worker", string(line)); len(got) != 1 || got[0].Kind != "task" {
		t.Fatalf("parseChildSession without known refs: %+v", got)
	}
}

func TestResumeFallsBackOnlyForUnresumableRuns(t *testing.T) {
	for _, r := range []relayReply{
		{Code: "not_found", Message: "x"},
		{Code: "invalid_state", Message: "Async run 'x' was stopped and cannot be resumed. Start a new run instead."},
		{Code: "execution_failed", Message: "Async run 'x' child 1 does not have a persisted session file to resume from."},
	} {
		if !resumeFallsBack(r) {
			t.Fatalf("no fallback for %+v", r)
		}
	}
	if resumeFallsBack(relayReply{Code: "invalid_params", Message: "RPC resume requires id"}) {
		t.Fatal("invalid parameters must not fall back")
	}
}

func TestTranscriptLines(t *testing.T) {
	for _, tc := range []struct {
		e    store.SubagentEntry
		want string
	}{
		{store.SubagentEntry{Kind: "task", Payload: json.RawMessage(`{"text":"Zähl bis 3"}`)}, "Task: Zähl bis 3"},
		{store.SubagentEntry{Kind: "text", Payload: json.RawMessage(`{"text":"1, 2, 3"}`)}, "Answer: 1, 2, 3"},
		{store.SubagentEntry{Kind: "user_echo", Payload: json.RawMessage(`{"text":"[Message from the user, sent directly to you · ref agw-0a1b2c3d]\n\nZehn"}`)}, "The user wrote: Zehn"},
		{store.SubagentEntry{Kind: "user_echo", Payload: json.RawMessage(`{"text":"` + restartHeader + `\n\nfence"}`)}, ""},
		{store.SubagentEntry{Kind: "tool_call", Payload: json.RawMessage(`{"name":"bash","arguments":{"command":"ls"}}`)}, `Tool call: bash {"command":"ls"}`},
		{store.SubagentEntry{Kind: "tool_result", Payload: json.RawMessage(`{"text":"ok"}`)}, "Result: ok"},
	} {
		if got := transcriptLine(tc.e); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.e.Kind, got, tc.want)
		}
	}
}

func TestClipToKeepsCharacters(t *testing.T) {
	if got := clipTo("äöü", 3); got != "ä …" {
		t.Fatalf("clip cut a character: %q", got)
	}
	if got := clipTo("kurz", 10); got != "kurz" {
		t.Fatalf("short text changed: %q", got)
	}
}

func TestOnlyPassiveAndSubagentHint(t *testing.T) {
	passive := store.QueueEntry{Kind: store.QueueSystem, Note: store.NoteSubagentUser, Text: "The user wrote …\nhallo"}
	background := store.QueueEntry{Kind: store.QueueSystem, Note: store.NoteBackground, Text: "bg"}
	user := store.QueueEntry{Kind: store.QueueUser, Text: "hallo"}
	if !onlyPassive([]store.QueueEntry{passive, passive}) || onlyPassive([]store.QueueEntry{passive, background}) || onlyPassive([]store.QueueEntry{user}) || onlyPassive(nil) {
		t.Fatal("onlyPassive misjudges the queue")
	}
	n := noteOf(passive)
	if n.FenceHint != subagentUserHint || n.Summary != "The user wrote …" {
		t.Fatalf("passive note read back: %+v", n)
	}
}

func TestRelayRefLayout(t *testing.T) {
	ref := newSubagentRef()
	if !relayRefRe.MatchString("[Message from the user, sent directly to you · ref "+ref+"]") || len(ref) != len("agw-")+8 {
		t.Fatalf("ref %q", ref)
	}
}

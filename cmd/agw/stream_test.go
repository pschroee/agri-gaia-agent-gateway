package main

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"testing"

	"agw/internal/agwclient"
)

func piEv(js string) agwclient.Event { return agwclient.Event{Kind: "pi", Data: []byte(js)} }

type decision struct {
	id      string
	approve bool
}

type fakeDecider struct {
	mu    sync.Mutex
	calls []decision
}

func (f *fakeDecider) decide(_ context.Context, id string, approve bool) (agwclient.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, decision{id, approve})
	st := "rejected"
	if approve {
		st = "approved"
	}
	return agwclient.Approval{ID: id, State: st}, nil
}

func newTestStreamer(mode approvalMode, in string) (*streamer, *strings.Builder, *strings.Builder, *fakeDecider) {
	var out, errw strings.Builder
	fd := &fakeDecider{}
	s := newStreamer(&out, &errw)
	s.mode = mode
	s.chatID = "c1"
	s.decide = fd.decide
	s.in = bufio.NewReader(strings.NewReader(in))
	return s, &out, &errw, fd
}

func feed(t *testing.T, s *streamer, after bool, evs ...agwclient.Event) bool {
	t.Helper()
	done := false
	for _, ev := range evs {
		d, err := s.handle(context.Background(), ev, after)
		if err != nil {
			t.Fatalf("handle: %v", err)
		}
		done = done || d
	}
	return done
}

func TestRenderTextAndTools(t *testing.T) {
	s, out, errw, _ := newTestStreamer(approvalShow, "")
	feed(t, s, true,
		piEv(`{"type":"message_start","message":{"role":"assistant","content":[]}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello "}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":1,"delta":"secret"}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"world"}}`),
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`),
		piEv(`{"type":"tool_execution_start","toolCallId":"t1","toolName":"bash","args":{"command":"ls"}}`),
		piEv(`{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"a\nb\nc\nd\ne\nf\ng\nh\ni\nj"}]},"isError":false}`),
	)
	if out.String() != "Hello world\n" {
		t.Errorf("stdout = %q", out.String())
	}
	e := errw.String()
	if !strings.Contains(e, `▶ bash {"command":"ls"}`) {
		t.Errorf("tool line missing: %q", e)
	}
	if strings.Contains(e, "secret") || strings.Contains(out.String(), "secret") {
		t.Errorf("thinking printed without --thinking")
	}
	if !strings.Contains(e, "a") || strings.Contains(e, "\n  │ j") {
		t.Errorf("result not truncated: %q", e)
	}
	if !strings.Contains(e, "more lines") {
		t.Errorf("truncation note missing: %q", e)
	}
}

func TestThinkingShownWhenEnabled(t *testing.T) {
	s, out, errw, _ := newTestStreamer(approvalShow, "")
	s.thinking = true
	feed(t, s, true,
		piEv(`{"type":"message_start","message":{"role":"assistant"}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"pondering"}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`),
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[]}}`),
	)
	if !strings.Contains(errw.String(), "pondering") {
		t.Errorf("thinking missing: %q", errw.String())
	}
	if out.String() != "ok\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestMessageEndWithoutDeltasPrintsText(t *testing.T) {
	s, out, _, _ := newTestStreamer(approvalShow, "")
	feed(t, s, true,
		piEv(`{"type":"message_start","message":{"role":"assistant"}}`),
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`),
		piEv(`{"type":"message_start","message":{"role":"system"}}`),
		piEv(`{"type":"message_end","message":{"role":"system","content":[{"type":"text","text":"internal"}]}}`),
	)
	if out.String() != "done\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAssistantErrorMarksFailure(t *testing.T) {
	s, _, errw, _ := newTestStreamer(approvalShow, "")
	feed(t, s, true,
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"quota exhausted"}}`),
		agwclient.Event{Kind: "error", Data: []byte(`{"message":"sandbox gone"}`)},
	)
	if !s.failed {
		t.Error("failed should be set")
	}
	if !strings.Contains(errw.String(), "quota exhausted") || !strings.Contains(errw.String(), "sandbox gone") {
		t.Errorf("error not printed: %q", errw.String())
	}
}

func TestEndDetection(t *testing.T) {
	s, _, _, _ := newTestStreamer(approvalShow, "")
	// A run that ended before sending does not count.
	if feed(t, s, false, piEv(`{"type":"agent_start"}`), piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("events before sending must not end the waiting")
	}
	// agent_settled after sending, but without agent_start afterwards: not done yet.
	if feed(t, s, true, piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("agent_settled without agent_start after sending must not end the waiting")
	}
	if feed(t, s, true, piEv(`{"type":"agent_start"}`), piEv(`{"type":"agent_end"}`)) {
		t.Fatal("agent_end is not the end")
	}
	if !feed(t, s, true, piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("agent_settled after agent_start must end the waiting")
	}
}

func TestEndDetectionAlreadyRunning(t *testing.T) {
	// If pi is already running when sending (steer), there is no new agent_start.
	s, _, _, _ := newTestStreamer(approvalShow, "")
	s.sawStart = true
	if !feed(t, s, true, piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("with a running chat the next agent_settled ends the waiting")
	}
}

const pendingApproval = `{"id":"a1","chat_id":"c1","kind":"artifact_upload","via":"cli","name":"report.md","size":120,"state":"pending","preview":"# Report"}`

func TestAutoApprove(t *testing.T) {
	s, _, errw, fd := newTestStreamer(approvalAuto, "")
	ev := agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)}
	feed(t, s, true, ev, ev,
		agwclient.Event{Kind: "approval", Data: []byte(`{"id":"a2","chat_id":"other","name":"x","state":"pending"}`)},
		agwclient.Event{Kind: "approval", Data: []byte(`{"id":"a1","chat_id":"c1","name":"report.md","state":"approved"}`)},
	)
	if len(fd.calls) != 1 || fd.calls[0] != (decision{"a1", true}) {
		t.Fatalf("decisions = %+v", fd.calls)
	}
	if !strings.Contains(errw.String(), "report.md") || !strings.Contains(errw.String(), "approved") {
		t.Errorf("output = %q", errw.String())
	}
}

func TestAutoReject(t *testing.T) {
	s, _, _, fd := newTestStreamer(approvalReject, "")
	feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)})
	if len(fd.calls) != 1 || fd.calls[0] != (decision{"a1", false}) {
		t.Fatalf("decisions = %+v", fd.calls)
	}
}

func TestAskApproval(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{{"y\n", true}, {"Yes\n", true}, {"n\n", false}, {"maybe\nn\n", false}} {
		s, _, errw, fd := newTestStreamer(approvalAsk, tc.in)
		feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)})
		if !strings.Contains(errw.String(), "Approve artifact report.md (120 bytes)? [y/n]") {
			t.Errorf("question missing: %q", errw.String())
		}
		if len(fd.calls) != 1 || fd.calls[0].approve != tc.want {
			t.Errorf("input %q: decisions = %+v", tc.in, fd.calls)
		}
	}
}

func TestAskApprovalEOFLeavesOpen(t *testing.T) {
	s, _, errw, fd := newTestStreamer(approvalAsk, "")
	feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)})
	if len(fd.calls) != 0 {
		t.Fatalf("expected no decision without input: %+v", fd.calls)
	}
	if !strings.Contains(errw.String(), "agw approve a1") {
		t.Errorf("hint missing: %q", errw.String())
	}
}

func TestFollowStopsAtSettled(t *testing.T) {
	s, out, _, _ := newTestStreamer(approvalShow, "")
	sse := ": ping\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_start\"}}\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"x\"}}}\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_settled\"}}\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"AFTERWARDS\"}}}\n\n"
	if err := follow(context.Background(), strings.NewReader(sse), s, nil, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "AFTERWARDS") {
		t.Errorf("kept reading after the end: %q", out.String())
	}
}

func TestFollowEOFBeforeEnd(t *testing.T) {
	s, _, _, _ := newTestStreamer(approvalShow, "")
	err := follow(context.Background(), strings.NewReader("data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_start\"}}\n\n"), s, nil, true)
	if err == nil || !strings.Contains(err.Error(), "broke off") {
		t.Errorf("expected an error: %v", err)
	}
}

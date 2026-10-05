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
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hallo "}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":1,"delta":"geheim"}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Welt"}}`),
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hallo Welt"}]}}`),
		piEv(`{"type":"tool_execution_start","toolCallId":"t1","toolName":"bash","args":{"command":"ls"}}`),
		piEv(`{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"a\nb\nc\nd\ne\nf\ng\nh\ni\nj"}]},"isError":false}`),
	)
	if out.String() != "Hallo Welt\n" {
		t.Errorf("stdout = %q", out.String())
	}
	e := errw.String()
	if !strings.Contains(e, `▶ bash {"command":"ls"}`) {
		t.Errorf("Werkzeugzeile fehlt: %q", e)
	}
	if strings.Contains(e, "geheim") || strings.Contains(out.String(), "geheim") {
		t.Errorf("Thinking ohne --thinking ausgegeben")
	}
	if !strings.Contains(e, "a") || strings.Contains(e, "\n  │ j") {
		t.Errorf("Ergebnis nicht gekürzt: %q", e)
	}
	if !strings.Contains(e, "weitere Zeilen") {
		t.Errorf("Kürzungshinweis fehlt: %q", e)
	}
}

func TestThinkingShownWhenEnabled(t *testing.T) {
	s, out, errw, _ := newTestStreamer(approvalShow, "")
	s.thinking = true
	feed(t, s, true,
		piEv(`{"type":"message_start","message":{"role":"assistant"}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"überlege"}}`),
		piEv(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`),
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[]}}`),
	)
	if !strings.Contains(errw.String(), "überlege") {
		t.Errorf("Thinking fehlt: %q", errw.String())
	}
	if out.String() != "ok\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestMessageEndWithoutDeltasPrintsText(t *testing.T) {
	s, out, _, _ := newTestStreamer(approvalShow, "")
	feed(t, s, true,
		piEv(`{"type":"message_start","message":{"role":"assistant"}}`),
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"fertig"}]}}`),
		piEv(`{"type":"message_start","message":{"role":"system"}}`),
		piEv(`{"type":"message_end","message":{"role":"system","content":[{"type":"text","text":"intern"}]}}`),
	)
	if out.String() != "fertig\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAssistantErrorMarksFailure(t *testing.T) {
	s, _, errw, _ := newTestStreamer(approvalShow, "")
	feed(t, s, true,
		piEv(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Kontingent erschöpft"}}`),
		agwclient.Event{Kind: "error", Data: []byte(`{"message":"Sandbox weg"}`)},
	)
	if !s.failed {
		t.Error("failed sollte gesetzt sein")
	}
	if !strings.Contains(errw.String(), "Kontingent erschöpft") || !strings.Contains(errw.String(), "Sandbox weg") {
		t.Errorf("Fehler nicht ausgegeben: %q", errw.String())
	}
}

func TestEndDetection(t *testing.T) {
	s, _, _, _ := newTestStreamer(approvalShow, "")
	// Ein Lauf, der vor dem Senden endete, zählt nicht.
	if feed(t, s, false, piEv(`{"type":"agent_start"}`), piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("Ereignisse vor dem Senden dürfen nicht beenden")
	}
	// agent_settled nach dem Senden, aber ohne agent_start danach: noch nicht fertig.
	if feed(t, s, true, piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("agent_settled ohne agent_start nach dem Senden darf nicht beenden")
	}
	if feed(t, s, true, piEv(`{"type":"agent_start"}`), piEv(`{"type":"agent_end"}`)) {
		t.Fatal("agent_end ist nicht das Ende")
	}
	if !feed(t, s, true, piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("agent_settled nach agent_start muss beenden")
	}
}

func TestEndDetectionAlreadyRunning(t *testing.T) {
	// Läuft pi beim Senden schon (steer), gibt es kein neues agent_start.
	s, _, _, _ := newTestStreamer(approvalShow, "")
	s.sawStart = true
	if !feed(t, s, true, piEv(`{"type":"agent_settled"}`)) {
		t.Fatal("bei laufendem Chat beendet das nächste agent_settled")
	}
}

const pendingApproval = `{"id":"a1","chat_id":"c1","kind":"artifact_upload","via":"cli","name":"bericht.md","size":120,"state":"pending","preview":"# Bericht"}`

func TestAutoApprove(t *testing.T) {
	s, _, errw, fd := newTestStreamer(approvalAuto, "")
	ev := agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)}
	feed(t, s, true, ev, ev,
		agwclient.Event{Kind: "approval", Data: []byte(`{"id":"a2","chat_id":"anderer","name":"x","state":"pending"}`)},
		agwclient.Event{Kind: "approval", Data: []byte(`{"id":"a1","chat_id":"c1","name":"bericht.md","state":"approved"}`)},
	)
	if len(fd.calls) != 1 || fd.calls[0] != (decision{"a1", true}) {
		t.Fatalf("Entscheidungen = %+v", fd.calls)
	}
	if !strings.Contains(errw.String(), "bericht.md") || !strings.Contains(errw.String(), "bestätigt") {
		t.Errorf("Ausgabe = %q", errw.String())
	}
}

func TestAutoReject(t *testing.T) {
	s, _, _, fd := newTestStreamer(approvalReject, "")
	feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)})
	if len(fd.calls) != 1 || fd.calls[0] != (decision{"a1", false}) {
		t.Fatalf("Entscheidungen = %+v", fd.calls)
	}
}

func TestAskApproval(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{{"j\n", true}, {"Ja\n", true}, {"n\n", false}, {"vielleicht\nn\n", false}} {
		s, _, errw, fd := newTestStreamer(approvalAsk, tc.in)
		feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)})
		if !strings.Contains(errw.String(), "Artefakt bericht.md (120 Bytes) bestätigen? [j/n]") {
			t.Errorf("Frage fehlt: %q", errw.String())
		}
		if len(fd.calls) != 1 || fd.calls[0].approve != tc.want {
			t.Errorf("Eingabe %q: Entscheidungen = %+v", tc.in, fd.calls)
		}
	}
}

func TestAskApprovalEOFLeavesOpen(t *testing.T) {
	s, _, errw, fd := newTestStreamer(approvalAsk, "")
	feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(pendingApproval)})
	if len(fd.calls) != 0 {
		t.Fatalf("ohne Eingabe keine Entscheidung erwartet: %+v", fd.calls)
	}
	if !strings.Contains(errw.String(), "agw approve a1") {
		t.Errorf("Hinweis fehlt: %q", errw.String())
	}
}

func TestFollowStopsAtSettled(t *testing.T) {
	s, out, _, _ := newTestStreamer(approvalShow, "")
	sse := ": ping\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_start\"}}\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"x\"}}}\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_settled\"}}\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"DANACH\"}}}\n\n"
	if err := follow(context.Background(), strings.NewReader(sse), s, nil, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "DANACH") {
		t.Errorf("nach dem Ende weitergelesen: %q", out.String())
	}
}

func TestFollowEOFBeforeEnd(t *testing.T) {
	s, _, _, _ := newTestStreamer(approvalShow, "")
	err := follow(context.Background(), strings.NewReader("data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_start\"}}\n\n"), s, nil, true)
	if err == nil || !strings.Contains(err.Error(), "abgebrochen") {
		t.Errorf("Fehler erwartet: %v", err)
	}
}

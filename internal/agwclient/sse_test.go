package agwclient

import (
	"strings"
	"testing"
)

func collect(t *testing.T, in string) []Event {
	t.Helper()
	var got []Event
	if err := ReadEvents(strings.NewReader(in), func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	return got
}

func TestReadEventsBasic(t *testing.T) {
	in := ": ping\n\n" +
		"data: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_start\"}}\n\n" +
		"data: {\"kind\":\"chat\",\"data\":{\"id\":\"c1\"}}\r\n\r\n"
	got := collect(t, in)
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "pi" || got[0].PiType() != "agent_start" {
		t.Errorf("first event wrong: %+v", got[0])
	}
	if got[1].Kind != "chat" {
		t.Errorf("second event wrong: %+v", got[1])
	}
}

func TestReadEventsMultilineAndTrailing(t *testing.T) {
	// Several data lines are joined with \n; an event without a trailing blank line
	// at the end of the stream is still delivered; invalid JSON is skipped.
	in := "data: {\"kind\":\"error\",\n" +
		"data: \"data\":{\"message\":\"x\"}}\n\n" +
		"event: foo\nid: 3\ndata: broken\n\n" +
		"data: {\"kind\":\"artifact\",\"data\":{}}"
	got := collect(t, in)
	if len(got) != 2 || got[0].Kind != "error" || got[1].Kind != "artifact" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestPiTypeOnlyForPi(t *testing.T) {
	ev := Event{Kind: "chat", Data: []byte(`{"type":"agent_start"}`)}
	if ev.PiType() != "" {
		t.Errorf("PiType for kind=chat should be empty")
	}
}

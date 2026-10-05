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
		t.Fatalf("2 Ereignisse erwartet, %d erhalten: %+v", len(got), got)
	}
	if got[0].Kind != "pi" || got[0].PiType() != "agent_start" {
		t.Errorf("erstes Ereignis falsch: %+v", got[0])
	}
	if got[1].Kind != "chat" {
		t.Errorf("zweites Ereignis falsch: %+v", got[1])
	}
}

func TestReadEventsMultilineAndTrailing(t *testing.T) {
	// Mehrere data-Zeilen werden mit \n verbunden; ein Ereignis ohne abschließende Leerzeile
	// am Stromende wird trotzdem geliefert; ungültiges JSON wird übersprungen.
	in := "data: {\"kind\":\"error\",\n" +
		"data: \"data\":{\"message\":\"x\"}}\n\n" +
		"event: foo\nid: 3\ndata: kaputt\n\n" +
		"data: {\"kind\":\"artifact\",\"data\":{}}"
	got := collect(t, in)
	if len(got) != 2 || got[0].Kind != "error" || got[1].Kind != "artifact" {
		t.Fatalf("unerwartet: %+v", got)
	}
}

func TestPiTypeOnlyForPi(t *testing.T) {
	ev := Event{Kind: "chat", Data: []byte(`{"type":"agent_start"}`)}
	if ev.PiType() != "" {
		t.Errorf("PiType bei kind=chat sollte leer sein")
	}
}

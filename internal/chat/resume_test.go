package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

// collect liest Ereignisse, bis stop wahr ist oder die Frist abläuft.
func collect(t *testing.T, ch <-chan Event, stop func(Event) bool) []Event {
	t.Helper()
	var out []Event
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
			if stop(ev) {
				return out
			}
		case <-timeout:
			t.Fatalf("Ereignis kam nicht; bisher %d", len(out))
		}
	}
}

func isPi(ev Event, typ string) bool {
	if ev.Kind != "pi" {
		return false
	}
	var h struct{ Type string }
	_ = json.Unmarshal(ev.Data.(json.RawMessage), &h)
	return h.Type == typ
}

// resumeSteps liefert „phase:status“ der resume-Ereignisse und den Index des ersten pi-Ereignisses.
func resumeSteps(evs []Event) (steps []string, byPhase map[string]ResumeStep, firstPi int, chatResuming bool) {
	firstPi = -1
	byPhase = map[string]ResumeStep{}
	for i, ev := range evs {
		switch ev.Kind {
		case "resume":
			s := ev.Data.(ResumeStep)
			steps = append(steps, s.Phase+":"+s.Status)
			if s.Status != "running" {
				byPhase[s.Phase] = s
			}
			if firstPi >= 0 {
				steps = append(steps, "NACH-PI")
			}
		case "pi":
			if firstPi < 0 {
				firstPi = i
			}
		case "chat":
			if ev.Data.(ChatView).Resuming {
				chatResuming = true
			}
		}
	}
	return
}

func TestResumeReportsStepsBeforeFirstAnswer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Merke dir 42"})
	waitSettled(t, e, c.ID)
	if _, err := e.m.AddInput(ctx, c.ID, "daten.csv", []byte("a,b\n1,2\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	res, err := e.m.Send(ctx, c.ID, "Was war die Zahl?")
	if err != nil || !res.Resumed || res.Queued {
		t.Fatalf("Senden: %+v %v", res, err)
	}
	evs := collect(t, events, func(ev Event) bool { return isPi(ev, "agent_settled") })
	steps, by, firstPi, resuming := resumeSteps(evs)
	want := "acquire:running,acquire:done,session:running,session:done,settings:running,settings:done," +
		"workspace:running,workspace:done,inputs:running,inputs:done,ready:done"
	if got := strings.Join(steps, ","); got != want {
		t.Fatalf("Schritte:\n%s\nerwartet\n%s", got, want)
	}
	if firstPi < 0 {
		t.Fatal("keine Antwort")
	}
	if !resuming {
		t.Error("Chat-Ereignis mit resuming=true fehlt")
	}
	if s := by[PhaseSession]; s.Size == nil || *s.Size == 0 {
		t.Errorf("Sitzung ohne Größe: %+v", s)
	}
	if s := by[PhaseInputs]; s.Files == nil || *s.Files != 1 || s.Size == nil || *s.Size != 8 {
		t.Errorf("Eingaben: %+v", s)
	}
	if s := by[PhaseWorkspace]; s.Detail != "keine Sicherung" {
		t.Errorf("Arbeitsbereich: %+v", s)
	}
	if s := by[PhaseReady]; s.ID == "" || s.ID != by[PhaseAcquire].ID {
		t.Errorf("Kennung: %+v / %+v", s, by[PhaseAcquire])
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.Resuming {
		t.Error("nach dem Fortsetzen noch resuming")
	}
	// Ein aktiver Chat meldet beim Senden keine Schritte.
	events2, cancel2 := e.m.Subscribe(c.ID)
	defer cancel2()
	if _, err := e.m.Send(ctx, c.ID, "Und jetzt?"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range collect(t, events2, func(ev Event) bool { return isPi(ev, "agent_settled") }) {
		if ev.Kind == "resume" {
			t.Fatalf("Schritt bei aktivem Chat: %+v", ev.Data)
		}
	}
}

func TestResumeFailureReportsFailed(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "eins"})
	waitSettled(t, e, c.ID)
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.failSwitch = true
	for _, a := range e.agents {
		a.mu.Lock()
		a.failSwitch = true
		a.mu.Unlock()
	}
	e.mu.Unlock()
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if _, err := e.m.Send(ctx, c.ID, "zwei"); err == nil {
		t.Fatal("Fortsetzen hätte scheitern müssen")
	}
	evs := collect(t, events, func(ev Event) bool {
		s, ok := ev.Data.(ResumeStep)
		return ok && s.Phase == PhaseFailed
	})
	steps, by, firstPi, _ := resumeSteps(evs)
	want := "acquire:running,acquire:done,session:running,session:error,failed:error"
	if got := strings.Join(steps, ","); got != want {
		t.Fatalf("Schritte:\n%s\nerwartet\n%s", got, want)
	}
	if firstPi >= 0 {
		t.Error("pi-Ereignis trotz gescheitertem Fortsetzen")
	}
	if !strings.Contains(by[PhaseFailed].Detail, "switch_session") {
		t.Errorf("Grund: %q", by[PhaseFailed].Detail)
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.State != store.StateDormant || v.Resuming || v.SlotID != "" {
		t.Fatalf("nach Fehler: %+v", v)
	}
	// Die Nachricht ist nicht gesendet und nicht gespeichert; sie ist auch nicht eingereiht.
	msgs, _ := e.st.Messages(ctx, c.ID)
	if len(msgs) != 2 {
		t.Fatalf("Nachrichten: %d", len(msgs))
	}
	if q, _ := e.m.Queue(ctx, c.ID); len(q) != 0 {
		t.Fatalf("eingereiht: %+v", q)
	}
}

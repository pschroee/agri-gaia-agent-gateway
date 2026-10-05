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
		{[]store.QueueEntry{{Text: "eins"}}, "eins"},
		{[]store.QueueEntry{{Text: "eins"}, {Text: " zwei "}}, "eins\n\nzwei"},
		{[]store.QueueEntry{{Text: "a", Attachments: []string{"x.csv"}}}, "a\n\n[Anhänge unter /workspace/inputs/]\n- x.csv"},
		{[]store.QueueEntry{{Attachments: []string{"x.csv"}}}, "Siehe Anhänge.\n\n[Anhänge unter /workspace/inputs/]\n- x.csv"},
		{[]store.QueueEntry{{Text: "a", Attachments: []string{"x.csv"}}, {Text: "b", Attachments: []string{"y.png", "x.csv"}}},
			"a\n\nb\n\n[Anhänge unter /workspace/inputs/]\n- x.csv\n- y.png"},
	}
	for _, c := range cases {
		if got := composeMessage(c.in, nil); got.Text != c.want || got.Origin != store.OriginUser {
			t.Errorf("composeMessage(%+v) = %q, erwartet %q", c.in, got, c.want)
		}
	}
}

// prompts liefert die Aufträge, die an den Agenten gingen.
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
	t.Fatalf("Zeitüberschreitung: %s", what)
}

// busyChat legt einen Chat an, dessen erster Lauf erst endet, wenn release aufgerufen wird.
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
	res, err := e.m.Send(ctx, c.ID, "eins")
	if err != nil || res.Queued {
		t.Fatalf("erste Nachricht: %+v %v", res, err)
	}
	return c.ID, a, func() { close(hold) }
}

func TestQueueWhileRunningDeliveredTogether(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, a, release := busyChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	if _, err := e.m.AddInput(ctx, id, "daten.csv", []byte("x")); err != nil {
		t.Fatal(err)
	}
	r2, err := e.m.SendWithAttachments(ctx, id, "zwei", []string{"daten.csv"})
	if err != nil || !r2.Queued || r2.QueueID == "" {
		t.Fatalf("zweite: %+v %v", r2, err)
	}
	r3, _ := e.m.Send(ctx, id, "drei")
	r4, _ := e.m.Send(ctx, id, "vier")
	if !r3.Queued || !r4.Queued {
		t.Fatalf("nicht eingereiht: %+v %+v", r3, r4)
	}
	ev := waitEvent(t, events, "queue", "")
	if qe := ev.Data.(QueueEvent); qe.Change != "queued" || len(qe.Entries) != 1 {
		t.Fatalf("Ereignis: %+v", qe)
	}
	if err := e.m.Unqueue(ctx, id, r3.QueueID); err != nil {
		t.Fatal(err)
	}
	v, _ := e.m.View(ctx, id)
	if v.Queued != 2 || v.QueueHeld {
		t.Fatalf("während des Laufs: queued=%d held=%v", v.Queued, v.QueueHeld)
	}
	if got := a.prompts(); len(got) != 1 {
		t.Fatalf("vor Laufende schon übergeben: %q", got)
	}
	release()
	waitUntil(t, "Übergabe", func() bool { return len(a.prompts()) == 2 })
	want := "zwei\n\nvier\n\n[Anhänge unter /workspace/inputs/]\n- daten.csv"
	if got := a.prompts()[1]; got != want {
		t.Fatalf("Auftrag:\n%q\nerwartet\n%q", got, want)
	}
	// Das Ereignis „delivered“ nennt den Text, damit die UI die Blase ohne Lücke zeigen kann.
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
		t.Fatalf("Entfernen nach Übergabe: %v", err)
	}
	waitSettled(t, e, id)
	if q, _ := e.m.Queue(ctx, id); len(q) != 0 {
		t.Fatalf("Warteschlange nicht leer: %+v", q)
	}
}

func TestQueueHeldAfterAbort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, a, release := busyChat(t, e)
	if r, _ := e.m.Send(ctx, id, "zwei"); !r.Queued {
		t.Fatal("nicht eingereiht")
	}
	if _, err := e.m.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}
	release()
	waitSettled(t, e, id)
	time.Sleep(100 * time.Millisecond) // Hintergrundarbeit nach agent_settled
	if got := a.prompts(); len(got) != 1 {
		t.Fatalf("nach Abbruch übergeben: %q", got)
	}
	v, _ := e.m.View(ctx, id)
	if v.Queued != 1 || !v.QueueHeld {
		t.Fatalf("nach Abbruch: queued=%d held=%v", v.Queued, v.QueueHeld)
	}
	// Die nächste Nachricht nimmt die zurückgehaltene mit.
	res, err := e.m.Send(ctx, id, "drei")
	if err != nil || res.Queued {
		t.Fatalf("drei: %+v %v", res, err)
	}
	if got := a.prompts(); len(got) != 2 || got[1] != "zwei\n\ndrei" {
		t.Fatalf("Aufträge: %q", got)
	}
	waitSettled(t, e, id)
	if v, _ := e.m.View(ctx, id); v.Queued != 0 || v.QueueHeld {
		t.Fatalf("danach: %+v", v)
	}
	// Ohne Eingereihtes gibt es nichts zu übergeben.
	if _, err := e.m.FlushQueue(ctx, id); !errors.Is(err, ErrInvalid) {
		t.Fatalf("leere Warteschlange: %v", err)
	}
}

// Die Warteschlange liegt in Postgres und übersteht einen Neustart des Orchestrators; danach
// ruht der Chat, und „jetzt senden“ setzt ihn mit den eingereihten Nachrichten fort.
func TestQueueSurvivesRestart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	if r, _ := e.m.Send(ctx, id, "nach dem Neustart"); !r.Queued {
		t.Fatal("nicht eingereiht")
	}
	// Neustart: neuer Manager auf derselben Datenbank; der alte Chat ruht.
	e.m.Shutdown(ctx)
	release()
	m2 := NewManager(e.st, e.p, e.cat, e.blobs, artifacts.NewBroker(), e.opt)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := m2.View(ctx, id)
	if v.State != store.StateDormant || v.Queued != 1 || !v.QueueHeld {
		t.Fatalf("nach Neustart: state=%s queued=%d held=%v", v.State, v.Queued, v.QueueHeld)
	}
	res, err := m2.FlushQueue(ctx, id)
	if err != nil || !res.Resumed {
		t.Fatalf("jetzt senden: %+v %v", res, err)
	}
	fresh := resumedAgent(e)
	if fresh == nil {
		t.Fatal("keine frische Sandbox")
	}
	waitUntil(t, "Auftrag", func() bool {
		p := fresh.prompts()
		return len(p) == 1 && p[0] == "nach dem Neustart"
	})
	if q, _ := m2.Queue(ctx, id); len(q) != 0 {
		t.Fatalf("nicht übergeben: %+v", q)
	}
}

// Läuft ein Werkzeug, geht eine neue Nachricht gleich per steer an pi und wird nach dem Werkzeug im
// selben Durchgang eingefügt (wie Claude Code), nicht erst nach dem Ende des Durchgangs.
func TestQueueSteeredWhileToolRuns(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "s"})
	a := e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold, a.withTool = hold, true
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "eins"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "Werkzeug läuft", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		l := e.m.live[c.ID]
		return l != nil && l.toolsRunning == 1
	})
	r, err := e.m.Send(ctx, c.ID, "zwei")
	if err != nil || !r.Queued {
		t.Fatalf("zweite: %+v %v", r, err)
	}
	waitUntil(t, "eingeschleust", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.steered) == 1 && a.steered[0] == "zwei" })
	if q, _ := e.m.Queue(ctx, c.ID); len(q) != 0 {
		t.Fatalf("noch eingereiht: %+v", q)
	}
	close(hold)
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	// ein Durchgang: eins, zwei (eingefügt nach dem Werkzeug), dann die Antwort
	if strings.Join(roles, ",") != "user,user,assistant" {
		t.Fatalf("Verlauf: %v", roles)
	}
	if n := strings.Count(strings.Join(a.commands(), ","), "prompt"); n != 2 {
		t.Fatalf("prompt-Aufrufe: %d (%v)", n, a.commands())
	}
}

// Bricht der Nutzer ab, bevor pi Eingeschleustes einfügt, wird es wieder eingereiht (zurückgehalten)
// statt verloren zu gehen.
func TestSteeredRestoredAfterAbort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "s"})
	a := e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold, a.withTool = hold, true
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "eins"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "Werkzeug läuft", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		l := e.m.live[c.ID]
		return l != nil && l.toolsRunning == 1
	})
	if _, err := e.m.Send(ctx, c.ID, "zwei"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "eingeschleust", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.steered) == 1 })
	if _, err := e.m.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	close(hold)
	waitSettled(t, e, c.ID)
	waitUntil(t, "wieder eingereiht", func() bool { q, _ := e.m.Queue(ctx, c.ID); return len(q) == 1 && q[0].Text == "zwei" })
	if v, _ := e.m.View(ctx, c.ID); !v.QueueHeld {
		t.Fatalf("nicht zurückgehalten: %+v", v)
	}
}

// Reine Meldungen des Orchestrators werden nicht eingeschleust: Sie gehen am Laufende über
// deliverQueue (Weckruf mit seinen Grenzen, Review 3, H2).
func TestSystemNoteNotSteered(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "s"})
	a := e.agent(0)
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold, a.withTool = hold, true
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "eins"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "Werkzeug läuft", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		l := e.m.live[c.ID]
		return l != nil && l.toolsRunning == 1
	})
	if _, err := e.st.EnqueueKind(ctx, c.ID, store.QueueSystem, "bg-1 ist fertig", nil); err != nil {
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
		t.Fatalf("Meldung eingeschleust: %d", n)
	}
	if q, _ := e.m.Queue(ctx, c.ID); len(q) != 1 {
		t.Fatalf("Warteschlange: %+v", q)
	}
	close(hold)
	waitSettled(t, e, c.ID)
}

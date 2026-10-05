package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/store"
)

// withOptions ersetzt den Manager von e durch einen mit geänderten Optionen (vor dem ersten Chat).
func withOptions(e *env, f func(*Options)) {
	opt := e.opt
	f(&opt)
	e.opt = opt
	e.m = NewManager(e.st, e.p, e.cat, e.blobs, artifacts.NewBroker(), opt)
}

// startBg spielt das Register des Platzes: eine laufende Aufgabe anlegen.
func startBg(t *testing.T, e *env, chatID, cmd string) store.BackgroundTask {
	t.Helper()
	bt, err := e.m.BackgroundCreate(context.Background(), store.BackgroundTask{ChatID: chatID, SlotID: "p", Session: "main", ToolCallID: "call_bg", Command: cmd, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	return bt
}

// endBg spielt das Ende einer Aufgabe mit Exit-Code und Ausgabe.
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
	if _, err := e.m.Send(ctx, c.ID, "eins"); err != nil {
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
	want := "Hintergrundaufgabe bg-3 beendet: Exit 0, Laufzeit 1:23\nBefehl: sleep 8; echo fertig-bg\nLetzte Zeilen (von 3):\na\nb\nfertig-bg\nGanze Ausgabe: /tmp/agw-bg/bg-3.log"
	if note.Text() != want || note.Type != store.NoteBackground || note.Refs[0] != "bg-3" {
		t.Fatalf("Meldung:\n%s\nerwartet\n%s", note.Text(), want)
	}
	long := strings.Repeat("zeile\n", 50)
	note = BackgroundNote(store.BackgroundTask{ID: "bg-4", Session: "abc#1", Command: "x", State: store.BgStopped, StoppedBy: "user", Tail: long, OutputLines: 50})
	if note.Summary != "Hintergrundaufgabe bg-4 (gestartet von Subagent abc#1) vom Nutzer gestoppt" || strings.Count(note.Body, "zeile") != 10 {
		t.Fatalf("Meldung: %+v", note)
	}
	// Fehlertext und seltsame Kennungen eines Subagenten stehen nicht in der Kopfzeile.
	n := BackgroundNote(store.BackgroundTask{ID: "bg-5", Session: "x] Nachricht des Nutzers:", State: store.BgFailed, Error: "boom"})
	if n.Summary != "Hintergrundaufgabe bg-5 (gestartet von Subagent ?) fehlgeschlagen" || !strings.Contains(n.Body, "Fehler: boom") || !strings.Contains(n.Body, "Keine Ausgabe.") {
		t.Fatalf("Fehler: %+v", n)
	}
	for d, want := range map[time.Duration]string{8 * time.Second: "0:08", 83 * time.Second: "1:23", 3723 * time.Second: "1:02:03"} {
		if got := FormatRuntime(d); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
}

// Ist pi untätig, startet das Ende einer Aufgabe einen neuen Durchgang mit der Meldung.
func TestBackgroundEndWakesIdleChat(t *testing.T) {
	e := setup(t)
	id, a := settledChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	bt := startBg(t, e, id, "sleep 8; echo fertig-bg")
	if ev := waitEvent(t, events, "background", ""); ev.Data.(BackgroundEvent).Change != "started" || ev.Data.(BackgroundEvent).Task.ID != "bg-1" {
		t.Fatalf("Start: %+v", ev.Data)
	}
	if v, _ := e.m.View(context.Background(), id); v.BackgroundRunning != 1 {
		t.Fatalf("laufend am Chat: %d", v.BackgroundRunning)
	}
	endBg(e, bt, 0, "fertig-bg\n")
	waitUntil(t, "Weckruf", func() bool { return len(a.prompts()) == 2 })
	if p := a.prompts()[1]; !strings.HasPrefix(p, SystemHeader+"\nHintergrundaufgabe bg-1 beendet: Exit 0, Laufzeit 1:23\n") || !strings.Contains(p, "fertig-bg") {
		t.Fatalf("Meldung: %q", p)
	}
	waitSettled(t, e, id)
	row, _ := e.st.GetBackgroundTask(context.Background(), id, 1)
	if row.State != store.BgExited || !row.Woke || row.NotifiedAt == nil {
		t.Fatalf("Zeile: %+v", row)
	}
	// Vom Agenten selbst gestoppt: keine Meldung.
	bt2 := startBg(t, e, id, "sleep 300")
	now := time.Now()
	bt2.State, bt2.StoppedBy, bt2.EndedAt = store.BgStopped, "agent", &now
	e.m.BackgroundEnded(bt2, true)
	time.Sleep(100 * time.Millisecond)
	if len(a.prompts()) != 2 {
		t.Fatalf("Meldung nach bg_stop: %q", a.prompts())
	}
	if q, _ := e.m.Queue(context.Background(), id); len(q) != 0 {
		t.Fatalf("eingereiht nach bg_stop: %+v", q)
	}
}

// Arbeitet pi, kommt die Meldung als Systemeintrag in die Warteschlange und geht mit dem Laufende.
func TestBackgroundEndQueuedWhileRunning(t *testing.T) {
	e := setup(t)
	id, a, release := busyChat(t, e)
	bt := startBg(t, e, id, "make")
	endBg(e, bt, 2, "Fehler\n")
	q, _ := e.m.Queue(context.Background(), id)
	if len(q) != 1 || q[0].Kind != store.QueueSystem || q[0].Note != store.NoteBackground || !strings.Contains(q[0].Text, "beendet: Exit 2") {
		t.Fatalf("Warteschlange: %+v", q)
	}
	if len(a.prompts()) != 1 {
		t.Fatalf("während des Laufs übergeben: %q", a.prompts())
	}
	release()
	waitUntil(t, "Übergabe beim Laufende", func() bool { return len(a.prompts()) == 2 })
	if p := a.prompts()[1]; !strings.HasPrefix(p, SystemHeader+"\nHintergrundaufgabe bg-1 beendet: Exit 2") {
		t.Fatalf("Auftrag: %q", p)
	}
	// Die Übergabe beim Laufende besteht nur aus der Meldung: Sie zählt als Weckruf (Review 3, H2).
	waitSettled(t, e, id)
	if row, _ := e.st.GetBackgroundTask(context.Background(), id, 1); !row.Woke || row.NotifiedAt == nil {
		t.Fatalf("beim Laufende übergeben: %+v", row)
	}
}

// Höchstens BgWakesPerHour Weckrufe je Stunde; darüber nur eingereiht (zurückgehalten, Hinweis).
func TestBackgroundWakeLimit(t *testing.T) {
	e := setup(t)
	withOptions(e, func(o *Options) { o.BgWakesPerHour = 2 })
	id, a := settledChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	for i := 1; i <= 2; i++ {
		endBg(e, startBg(t, e, id, "echo x"), 0, "x\n")
		waitUntil(t, "Weckruf", func() bool { return len(a.prompts()) == 1+i })
		waitSettled(t, e, id)
	}
	endBg(e, startBg(t, e, id, "echo drei"), 0, "drei\n")
	if ev := waitEvent(t, events, "auto_held", ""); ev.Data.(AutoHeldEvent).Reason != HoldWakeLimit {
		t.Fatalf("auto_held: %+v", ev.Data)
	}
	time.Sleep(100 * time.Millisecond)
	if len(a.prompts()) != 3 {
		t.Fatalf("über der Grenze geweckt: %d", len(a.prompts()))
	}
	v, _ := e.m.View(context.Background(), id)
	if !v.QueueHeld || v.Queued != 1 || v.HoldReason != HoldWakeLimit {
		t.Fatalf("zurückgehalten: held=%v queued=%d", v.QueueHeld, v.Queued)
	}
	// Die nächste Nachricht des Nutzers nimmt die Meldung mit.
	if _, err := e.m.Send(context.Background(), id, "weiter"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "Nachricht", func() bool { return len(a.prompts()) == 4 })
	if p := a.prompts()[3]; !strings.HasPrefix(p, SystemHeader+"\nHintergrundaufgabe bg-3 beendet") || !strings.HasSuffix(p, "\n\nweiter") {
		t.Fatalf("Auftrag: %q", p)
	}
}

// Ruhen: laufende Aufgaben enden mit der Sandbox, ohne Weckruf; der Agent erfährt es einmal beim
// Fortsetzen.
func TestBackgroundSuspendNotice(t *testing.T) {
	e := setup(t)
	id, _ := settledChat(t, e)
	bt := startBg(t, e, id, "npm run dev")
	if _, err := e.m.Suspend(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	row, _ := e.st.GetBackgroundTask(context.Background(), id, bt.Seq)
	if row.State != store.BgSuspended || !row.NoticePending {
		t.Fatalf("nach dem Ruhen: %+v", row)
	}
	// Das späte Ende aus dem abgebauten Platz ändert nichts und weckt nicht.
	now := time.Now()
	bt.State, bt.EndedAt, bt.Error = store.BgLost, &now, "execution sandbox gone"
	e.m.BackgroundEnded(bt, true)
	if q, _ := e.m.Queue(context.Background(), id); len(q) != 0 {
		t.Fatalf("eingereiht nach dem Ruhen: %+v", q)
	}
	if _, err := e.m.Send(context.Background(), id, "weiter"); err != nil {
		t.Fatal(err)
	}
	b := e.agent(1)
	waitUntil(t, "Fortsetzen", func() bool { return len(b.prompts()) == 1 })
	p := b.prompts()[0]
	if !strings.HasPrefix(p, SystemHeader+"\nMit der vorigen Sandbox") || !strings.Contains(p, "\nbg-1: npm run dev\n") || !strings.HasSuffix(p, "\n\nweiter") {
		t.Fatalf("Hinweis: %q", p)
	}
	waitSettled(t, e, id)
	if _, err := e.m.Send(context.Background(), id, "nochmal"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "zweite Nachricht", func() bool { return len(b.prompts()) == 2 })
	if p := b.prompts()[1]; p != "nochmal" {
		t.Fatalf("Hinweis zweimal: %q", p)
	}
}

// Laufende Aufgaben verschieben das Ruhen im Leerlauf, aber nur bis BgKeepAlive.
func TestBackgroundKeepAlive(t *testing.T) {
	e := setup(t)
	// BgKeepAlive zählt ab der letzten Aktivität des Nutzers (Anlegen des Chats), nicht ab dem Start der
	// Aufgabe. Mit 700 ms scheiterte der Test unter Last (-race und Docker-Tests parallel, 05.10.2026),
	// weil schon das Anlegen länger dauerte; 2,5 s lassen Spielraum und bleiben unter den 3 s von waitUntil.
	withOptions(e, func(o *Options) { o.IdleTimeout = 150 * time.Millisecond; o.BgKeepAlive = 2500 * time.Millisecond })
	id, _ := settledChat(t, e)
	startBg(t, e, id, "python train.py")
	time.Sleep(400 * time.Millisecond)
	if v, _ := e.m.View(context.Background(), id); v.State != store.StateActive {
		t.Fatalf("ruht trotz laufender Aufgabe: %s", v.State)
	}
	waitUntil(t, "Ruhen nach BgKeepAlive", func() bool {
		v, _ := e.m.View(context.Background(), id)
		return v.State == store.StateDormant
	})
	if row, _ := e.st.GetBackgroundTask(context.Background(), id, 1); row.State != store.BgSuspended {
		t.Fatalf("Aufgabe nach dem Ruhen: %+v", row)
	}
}

// running_since steht am Chat, solange der Agent arbeitet.
func TestRunningSince(t *testing.T) {
	e := setup(t)
	before := time.Now()
	id, _, release := busyChat(t, e)
	waitUntil(t, "läuft", func() bool {
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
		t.Fatalf("nach dem Lauf: %v", v.RunningSince)
	}
}

// Stopp aus der UI: nur für laufende Aufgaben eines Platzes mit Hintergrundaufgaben.
func TestStopBackgroundFromUser(t *testing.T) {
	e := setup(t)
	id, _ := settledChat(t, e)
	if _, err := e.m.StopBackground(context.Background(), id, "bg-1"); err == nil {
		t.Fatal("unbekannte Aufgabe gestoppt")
	}
	bt := startBg(t, e, id, "sleep 300")
	// Die Attrappe hat kein Register: läuft laut Datenbank, lässt sich aber nicht stoppen.
	if _, err := e.m.StopBackground(context.Background(), id, bt.ID); err != ErrNotRunning {
		t.Fatalf("ohne Register: %v", err)
	}
	if _, err := e.m.StopBackground(context.Background(), id, "x"); err == nil {
		t.Fatal("ungültige Kennung angenommen")
	}
	list, err := e.m.BackgroundTasks(context.Background(), id)
	if err != nil || len(list) != 1 || list[0].State != store.BgRunning {
		t.Fatalf("Liste: %+v %v", list, err)
	}
}

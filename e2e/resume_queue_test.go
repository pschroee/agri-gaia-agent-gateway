package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// userTexts liefert die Texte aller gespeicherten Nutzernachrichten.
func userTexts(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, m := range getChat(t, id).Messages {
		if m.Role != "user" {
			continue
		}
		var u struct {
			Content []struct{ Text string } `json:"content"`
		}
		_ = json.Unmarshal(m.Message, &u)
		var b strings.Builder
		for _, c := range u.Content {
			b.WriteString(c.Text)
		}
		out = append(out, b.String())
	}
	return out
}

// Beim Fortsetzen eines ruhenden Chats meldet der Orchestrator die Schritte über SSE, und zwar in
// der Reihenfolge, in der er sie ausführt, und bevor pi antwortet (die UI zeigt sie live).
func TestResumeStepsBeforeAnswer(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	uploadFile(t, id, "werte.csv", "a,b\n1,2\n")
	ask(t, s, id, "Antworte nur mit OK.", nil)
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("Ruhen: %d", code)
	}
	from := s.len()
	var res struct {
		Resumed bool `json:"resumed"`
		Queued  bool `json:"queued"`
	}
	start := time.Now()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": "Antworte nur mit WEITER."}, &res); code != 200 {
		t.Fatalf("senden: %d", code)
	}
	if !res.Resumed || res.Queued {
		t.Fatalf("Antwort: %+v", res)
	}
	t.Logf("POST /messages mit Fortsetzen: %v", time.Since(start).Round(time.Millisecond))
	_, next := s.waitFor(t, from, 3*time.Minute, "agent_start", func(ev map[string]any) bool { return piType(ev) == "agent_start" })
	s.waitFor(t, next, 3*time.Minute, "agent_settled", func(ev map[string]any) bool { return piType(ev) == "agent_settled" })

	s.mu.Lock()
	evs := append([]map[string]any(nil), s.events[from:]...)
	s.mu.Unlock()
	var steps []string
	firstPi, readyAt, resumingAt := -1, -1, -1
	var workspace, inputs map[string]any
	for i, ev := range evs {
		switch ev["kind"] {
		case "resume":
			d := ev["data"].(map[string]any)
			steps = append(steps, d["phase"].(string)+":"+d["status"].(string))
			if d["phase"] == "ready" {
				readyAt = i
			}
			if d["status"] == "done" {
				switch d["phase"] {
				case "workspace":
					workspace = d
				case "inputs":
					inputs = d
				}
			}
		case "pi":
			if firstPi < 0 {
				firstPi = i
			}
		case "chat":
			if d, _ := ev["data"].(map[string]any); d["resuming"] == true && resumingAt < 0 {
				resumingAt = i
			}
		}
	}
	want := "acquire:running,acquire:done,session:running,session:done,settings:running,settings:done," +
		"workspace:running,workspace:done,inputs:running,inputs:done,ready:done"
	if got := strings.Join(steps, ","); got != want {
		t.Fatalf("Schritte:\n%s\nerwartet\n%s", got, want)
	}
	if readyAt < 0 || firstPi < 0 || readyAt > firstPi {
		t.Fatalf("ready (%d) nicht vor dem ersten pi-Ereignis (%d)", readyAt, firstPi)
	}
	if resumingAt < 0 || resumingAt > readyAt {
		t.Fatalf("Chat-Ereignis „resuming“ fehlt oder kommt zu spät (%d, ready %d)", resumingAt, readyAt)
	}
	if inputs == nil || inputs["files"] != float64(1) {
		t.Fatalf("Eingaben: %v", inputs)
	}
	if workspace == nil {
		t.Fatal("Arbeitsbereich ohne Abschluss")
	}
	if c := getChat(t, id).Chat; c.State != "active" {
		t.Fatalf("nach Fortsetzen: %+v", c)
	}
	mustContain(t, lastAssistantText(t, id), "WEITER", "Antwort nach dem Fortsetzen")
}

// Nachrichten während eines Laufs reiht der Orchestrator ein; eine lässt sich entfernen, die
// übrigen gehen beim Laufende gemeinsam als nächster Auftrag an pi.
func TestQueueWhileRunning(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{
		"text": "Führe mit bash genau diesen Befehl aus: sleep 15 && echo fertig. Antworte danach nur mit FERTIG.",
	}, nil); code != 200 {
		t.Fatalf("senden: %d", code)
	}
	s.waitFor(t, from, 3*time.Minute, "bash läuft", func(ev map[string]any) bool { return piType(ev) == "tool_execution_start" })

	type sendRes struct {
		Queued  bool   `json:"queued"`
		QueueID string `json:"queue_id"`
	}
	var a, b sendRes
	apfel := "Antworte nur mit dem Wort Apfel."
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": apfel}, &a); code != 200 || !a.Queued || a.QueueID == "" {
		t.Fatalf("Apfel nicht eingereiht: %d %+v", code, a)
	}
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": "Antworte nur mit dem Wort Birne."}, &b); code != 200 || !b.Queued {
		t.Fatalf("Birne nicht eingereiht: %d %+v", code, b)
	}
	var q []struct{ ID, Text string }
	if call(t, "GET", "/api/chats/"+id+"/queue", nil, &q); len(q) != 2 || q[0].ID != a.QueueID {
		t.Fatalf("Warteschlange: %+v", q)
	}
	if code := call(t, "DELETE", "/api/chats/"+id+"/queue/"+b.QueueID, nil, nil); code != 200 {
		t.Fatalf("Birne entfernen: %d", code)
	}
	if c := getChat(t, id).Chat; !c.Running {
		t.Fatal("Lauf war schon zu Ende, bevor die Warteschlange geprüft wurde (sleep zu kurz?)")
	}

	ev, next := s.waitFor(t, from, 3*time.Minute, "Übergabe", func(ev map[string]any) bool {
		d, _ := ev["data"].(map[string]any)
		return ev["kind"] == "queue" && d["change"] == "delivered"
	})
	d := ev["data"].(map[string]any)
	if d["text"] != apfel {
		t.Fatalf("übergeben: %v", d)
	}
	_, next = s.waitFor(t, next, 3*time.Minute, "agent_start", func(ev map[string]any) bool { return piType(ev) == "agent_start" })
	s.waitFor(t, next, 3*time.Minute, "agent_settled", func(ev map[string]any) bool { return piType(ev) == "agent_settled" })

	users := userTexts(t, id)
	if len(users) != 2 || users[1] != apfel {
		t.Fatalf("Nutzernachrichten: %q", users)
	}
	txt := lastAssistantText(t, id)
	mustContain(t, txt, "Apfel", "Antwort auf die eingereihte Nachricht")
	if strings.Contains(txt, "Birne") {
		t.Fatalf("entfernte Nachricht kam trotzdem an: %q", txt)
	}
	if code := call(t, "DELETE", "/api/chats/"+id+"/queue/"+a.QueueID, nil, nil); code != 409 {
		t.Fatalf("Entfernen nach Übergabe: %d statt 409", code)
	}
	if c := getChat(t, id).Chat; c.Running {
		t.Fatal("läuft noch")
	}
}

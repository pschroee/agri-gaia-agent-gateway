package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// userTexts returns the texts of all stored user messages.
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

// When resuming an idle chat the orchestrator reports the steps over SSE, in the order
// in which it performs them, and before pi replies (the UI shows them live).
func TestResumeStepsBeforeAnswer(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	uploadFile(t, id, "values.csv", "a,b\n1,2\n")
	ask(t, s, id, "Reply only with OK.", nil)
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("idle: %d", code)
	}
	from := s.len()
	var res struct {
		Resumed bool `json:"resumed"`
		Queued  bool `json:"queued"`
	}
	start := time.Now()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": "Reply only with CONTINUE."}, &res); code != 200 {
		t.Fatalf("send: %d", code)
	}
	if !res.Resumed || res.Queued {
		t.Fatalf("response: %+v", res)
	}
	t.Logf("POST /messages with resume: %v", time.Since(start).Round(time.Millisecond))
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
		t.Fatalf("steps:\n%s\nwant\n%s", got, want)
	}
	if readyAt < 0 || firstPi < 0 || readyAt > firstPi {
		t.Fatalf("ready (%d) not before the first pi event (%d)", readyAt, firstPi)
	}
	if resumingAt < 0 || resumingAt > readyAt {
		t.Fatalf("chat event \"resuming\" missing or too late (%d, ready %d)", resumingAt, readyAt)
	}
	if inputs == nil || inputs["files"] != float64(1) {
		t.Fatalf("inputs: %v", inputs)
	}
	if workspace == nil {
		t.Fatal("workspace without completion")
	}
	if c := getChat(t, id).Chat; c.State != "active" {
		t.Fatalf("after resuming: %+v", c)
	}
	mustContain(t, lastAssistantText(t, id), "CONTINUE", "reply after resuming")
}

// The orchestrator enqueues messages sent during a run; one can be removed, the
// rest go to pi together as the next task when the run ends.
func TestQueueWhileRunning(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{
		"text": "Run exactly this command with bash: sleep 15 && echo done. Then reply only with DONE.",
	}, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	s.waitFor(t, from, 3*time.Minute, "bash running", func(ev map[string]any) bool { return piType(ev) == "tool_execution_start" })

	type sendRes struct {
		Queued  bool   `json:"queued"`
		QueueID string `json:"queue_id"`
	}
	var a, b sendRes
	apfel := "Reply only with the word apple."
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": apfel}, &a); code != 200 || !a.Queued || a.QueueID == "" {
		t.Fatalf("apple not queued: %d %+v", code, a)
	}
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": "Reply only with the word pear."}, &b); code != 200 || !b.Queued {
		t.Fatalf("pear not queued: %d %+v", code, b)
	}
	var q []struct{ ID, Text string }
	if call(t, "GET", "/api/chats/"+id+"/queue", nil, &q); len(q) != 2 || q[0].ID != a.QueueID {
		t.Fatalf("queue: %+v", q)
	}
	if code := call(t, "DELETE", "/api/chats/"+id+"/queue/"+b.QueueID, nil, nil); code != 200 {
		t.Fatalf("remove pear: %d", code)
	}
	if c := getChat(t, id).Chat; !c.Running {
		t.Fatal("run had already ended before the queue was checked (sleep too short?)")
	}

	ev, next := s.waitFor(t, from, 3*time.Minute, "handover", func(ev map[string]any) bool {
		d, _ := ev["data"].(map[string]any)
		return ev["kind"] == "queue" && d["change"] == "delivered"
	})
	d := ev["data"].(map[string]any)
	if d["text"] != apfel {
		t.Fatalf("handed over: %v", d)
	}
	_, next = s.waitFor(t, next, 3*time.Minute, "agent_start", func(ev map[string]any) bool { return piType(ev) == "agent_start" })
	s.waitFor(t, next, 3*time.Minute, "agent_settled", func(ev map[string]any) bool { return piType(ev) == "agent_settled" })

	users := userTexts(t, id)
	if len(users) != 2 || users[1] != apfel {
		t.Fatalf("user messages: %q", users)
	}
	txt := lastAssistantText(t, id)
	mustContain(t, txt, "apple", "reply to the queued message")
	if strings.Contains(strings.ToLower(txt), "pear") {
		t.Fatalf("removed message arrived anyway: %q", txt)
	}
	if code := call(t, "DELETE", "/api/chats/"+id+"/queue/"+a.QueueID, nil, nil); code != 409 {
		t.Fatalf("remove after handover: %d instead of 409", code)
	}
	if c := getChat(t, id).Chat; c.Running {
		t.Fatal("still running")
	}
}

package e2e

import (
	"encoding/json"
	"testing"
)

// Aufgabenliste (rpiv-todo, Werkzeug todo): Der Agent legt für eine kleine Aufgabe in drei
// Schritten eine Liste an und arbeitet sie ab. Geprüft wird über die gespeicherten Nachrichten,
// die auch die UI nach dem Neuladen liest: Jedes todo-Ergebnis trägt in details den
// vollständigen Stand, und am Ende sind alle Aufgaben erledigt. Läuft in der MCP-Variante, weil
// todo dort ausdrücklich in der Werkzeugliste stehen muss (die anderen laden es ohne Liste).
func TestAgentTaskList(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	ask(t, s, id, "Lege mit dem Werkzeug todo eine Aufgabenliste mit genau drei Aufgaben an: "+
		"1) Datei eins.txt mit dem Inhalt 1 schreiben, 2) Datei zwei.txt mit dem Inhalt 2 schreiben, "+
		"3) beide Dateien mit read lesen. Arbeite sie der Reihe nach ab: jede Aufgabe vorher auf in_progress, "+
		"danach auf completed setzen. Antworte am Ende mit einem Satz.", nil)

	type task struct {
		ID      int    `json:"id"`
		Subject string `json:"subject"`
		Status  string `json:"status"`
	}
	var last []task
	results, inProgress := 0, 0
	for _, m := range getChat(t, id).Messages {
		if m.Role != "toolResult" {
			continue
		}
		var r struct {
			ToolName string `json:"toolName"`
			IsError  bool   `json:"isError"`
			Details  *struct {
				Tasks  []task `json:"tasks"`
				NextID int    `json:"nextId"`
				Error  string `json:"error"`
			} `json:"details"`
		}
		if err := json.Unmarshal(m.Message, &r); err != nil || r.ToolName != "todo" {
			continue
		}
		results++
		if r.Details == nil || r.Details.NextID < 1 {
			t.Fatalf("todo-Ergebnis ohne details (vollständiger Stand): %s", trunc(string(m.Message), 400))
		}
		if r.IsError || r.Details.Error != "" {
			t.Logf("todo abgewiesen: %s", r.Details.Error)
			continue
		}
		last = r.Details.Tasks
		for _, x := range last {
			if x.Status == "in_progress" {
				inProgress++
			}
		}
	}
	if results < 6 { // drei anlegen, mindestens drei abschließen
		t.Fatalf("nur %d todo-Ergebnisse; Aufrufe: %v", results, toolCalls(t, id))
	}
	if inProgress == 0 {
		t.Error("keine Aufgabe war je in Arbeit")
	}
	open := 0
	for _, x := range last {
		if x.Status != "completed" && x.Status != "deleted" {
			open++
		}
	}
	if len(last) < 3 || open > 0 {
		t.Fatalf("Endstand: %+v", last)
	}
	t.Logf("%d todo-Ergebnisse, Endstand %+v", results, last)
}

// Der Systemhinweis verlangt, den Status sauber zu führen: vor dem Beginn einer Aufgabe
// in_progress, sofort nach dem Abschluss completed, am Ende nichts offen. Die
// Aufforderung nennt die Status bewusst NICHT; geprüft wird, was der Systemhinweis allein bewirkt.
func TestTaskStatusFollowsSystemNote(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	ask(t, s, id, "Erledige diese drei Schritte der Reihe nach und führe dabei eine Aufgabenliste: "+
		"1) Schreibe a.txt mit dem Inhalt A. 2) Schreibe b.txt mit dem Inhalt B. 3) Lies beide Dateien. "+
		"Antworte am Ende mit einem Satz.", nil)

	type task struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	var snaps [][]task
	for _, m := range getChat(t, id).Messages {
		if m.Role != "toolResult" {
			continue
		}
		var r struct {
			ToolName string `json:"toolName"`
			IsError  bool   `json:"isError"`
			Details  *struct {
				Tasks []task `json:"tasks"`
			} `json:"details"`
		}
		if json.Unmarshal(m.Message, &r) != nil || r.ToolName != "todo" || r.IsError || r.Details == nil {
			continue
		}
		snaps = append(snaps, r.Details.Tasks)
	}
	if len(snaps) == 0 {
		t.Fatalf("keine Aufgabenliste geführt; Aufrufe: %v", toolCalls(t, id))
	}
	// Je Aufgabe: Index des ersten Stands mit in_progress und mit completed.
	started, done := map[int]int{}, map[int]int{}
	for i, snap := range snaps {
		busy := 0
		for _, x := range snap {
			switch x.Status {
			case "in_progress":
				busy++
				if _, ok := started[x.ID]; !ok {
					started[x.ID] = i
				}
			case "completed":
				if _, ok := done[x.ID]; !ok {
					done[x.ID] = i
				}
			}
		}
		// Mehrere gleichzeitig in Arbeit ist erlaubt, wenn der Agent unabhängige Schritte parallel
		// ausführt (so im Lauf vom 29.09.2026); geprüft wird die Reihenfolge je Aufgabe.
		_ = busy
	}
	for id, d := range done {
		st, ok := started[id]
		if !ok || st >= d {
			t.Errorf("Aufgabe %d: nicht vor dem Abschluss auf in_progress gesetzt (in Arbeit %v, erledigt %d)", id, st, d)
		}
	}
	// „Sofort“: keine zwei Aufgaben im selben Aufruf erledigt.
	perSnap := map[int]int{}
	for _, d := range done {
		perSnap[d]++
	}
	for i, n := range perSnap {
		if n > 1 {
			t.Errorf("Stand %d: %d Aufgaben gesammelt erledigt statt einzeln", i, n)
		}
	}
	last := snaps[len(snaps)-1]
	for _, x := range last {
		if x.Status != "completed" && x.Status != "deleted" {
			t.Errorf("Endstand offen: %+v", last)
			break
		}
	}
	t.Logf("%d Stände, begonnen %v, erledigt %v", len(snaps), started, done)
}

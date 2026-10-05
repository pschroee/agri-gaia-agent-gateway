package e2e

import (
	"encoding/json"
	"testing"
)

// Task list (rpiv-todo, tool todo): the agent creates a list for a small task in three
// steps and works through it. Checked via the stored messages, which the UI also reads
// after reloading: every todo result carries the complete state in details, and at the
// end all tasks are done. Runs in the MCP variant because todo must be listed explicitly
// in the tool list there (the others load it without a list).
func TestAgentTaskList(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	ask(t, s, id, "Use the tool todo to create a task list with exactly three tasks: "+
		"1) write file one.txt with the content 1, 2) write file two.txt with the content 2, "+
		"3) read both files with read. Work through them in order: set each task to in_progress before, "+
		"and to completed after. Reply with one sentence at the end.", nil)

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
			t.Fatalf("todo result without details (complete state): %s", trunc(string(m.Message), 400))
		}
		if r.IsError || r.Details.Error != "" {
			t.Logf("todo refused: %s", r.Details.Error)
			continue
		}
		last = r.Details.Tasks
		for _, x := range last {
			if x.Status == "in_progress" {
				inProgress++
			}
		}
	}
	if results < 6 { // three to create, at least three to complete
		t.Fatalf("only %d todo results; calls: %v", results, toolCalls(t, id))
	}
	if inProgress == 0 {
		t.Error("no task was ever in progress")
	}
	open := 0
	for _, x := range last {
		if x.Status != "completed" && x.Status != "deleted" {
			open++
		}
	}
	if len(last) < 3 || open > 0 {
		t.Fatalf("final state: %+v", last)
	}
	t.Logf("%d todo results, final state %+v", results, last)
}

// The system note demands keeping the status clean: in_progress before starting a task,
// completed immediately after finishing it, nothing open at the end. The
// prompt deliberately does NOT name the statuses; what is checked is what the system note alone achieves.
func TestTaskStatusFollowsSystemNote(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	ask(t, s, id, "Do these three steps in order and keep a task list while doing so: "+
		"1) Write a.txt with the content A. 2) Write b.txt with the content B. 3) Read both files. "+
		"Reply with one sentence at the end.", nil)

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
		t.Fatalf("no task list kept; calls: %v", toolCalls(t, id))
	}
	// Per task: index of the first state with in_progress and with completed.
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
		// Several in progress at once is allowed when the agent runs independent steps in parallel
		// (as in the run of 2026-09-29); what is checked is the order per task.
		_ = busy
	}
	for id, d := range done {
		st, ok := started[id]
		if !ok || st >= d {
			t.Errorf("task %d: not set to in_progress before completion (in progress %v, done %d)", id, st, d)
		}
	}
	// "Immediately": no two tasks completed in the same call.
	perSnap := map[int]int{}
	for _, d := range done {
		perSnap[d]++
	}
	for i, n := range perSnap {
		if n > 1 {
			t.Errorf("state %d: %d tasks completed together instead of one by one", i, n)
		}
	}
	last := snaps[len(snaps)-1]
	for _, x := range last {
		if x.Status != "completed" && x.Status != "deleted" {
			t.Errorf("final state open: %+v", last)
			break
		}
	}
	t.Logf("%d states, started %v, done %v", len(snaps), started, done)
}

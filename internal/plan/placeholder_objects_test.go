// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// #169: a placeholder points at the step that created the object, never at a step that only starts a job.
func TestBuildRefusesPlaceholderOnRun(t *testing.T) {
	raw := `{"title":"t","steps":[
 {"method":"POST","path":"/train/config","body":{"dataset_id":1}},
 {"tool":"start_training","args":{"train_container_id":"{{step1.id}}"}},
 {"tool":"model_from_training","args":{"train_container_id":"{{step2.id}}"}}]}`
	p, err := ParseProposal([]byte(raw), testLimits)
	if err == nil {
		_, err = Build(p, classify)
	}
	if err == nil || !strings.Contains(err.Error(), "starts a job and creates no object") {
		t.Fatalf("want the refusal of a placeholder on a start, got %v", err)
	}
}

func TestReferenced(t *testing.T) {
	steps := []store.PlanStep{
		{Idx: 1, Path: "/train/config"},
		{Idx: 2, Path: "/models/5"},
		{Idx: 3, Path: "/train/containers/{{step1.id}}/run"},
		{Idx: 4, Tool: "model_from_training", Args: []byte(`{"train_container_id":"{{step1.id}}"}`)},
	}
	if got := Referenced(steps, 1); got != 3 {
		t.Errorf("first referencing step: %d", got)
	}
	if got := Referenced(steps, 2); got != 0 {
		t.Errorf("unreferenced step: %d", got)
	}
	if got := Referenced(steps[:3], 3); got != 0 {
		t.Errorf("last step: %d", got)
	}
}

func TestSameShape(t *testing.T) {
	run := store.PlanStep{Method: "POST", Path: "/train/containers/{{step2.id}}/run"}
	if !SameShape(run, "", platform.Request{Method: "POST", Path: "/train/containers/63/run"}) {
		t.Error("a placeholder segment must fit any id")
	}
	if SameShape(run, "", platform.Request{Method: "POST", Path: "/train/containers/63/stop"}) {
		t.Error("another path matched")
	}
	if SameShape(run, "", platform.Request{Method: "GET", Path: "/train/containers/63/run"}) {
		t.Error("another method matched")
	}
	tool := store.PlanStep{Tool: "start_training"}
	if !SameShape(tool, "start_training", platform.Request{}) || SameShape(tool, "create_training", platform.Request{}) {
		t.Error("a tool step compares the tool")
	}
}

func TestWordingOfPreviewAndApproval(t *testing.T) {
	steps := build(t, twoSteps)
	if p := Preview("t", steps, nil); !strings.Contains(p, "counted again from the end of each step that waits for a platform job") {
		t.Errorf("preview: %s", p)
	}
	text := ApprovedText(store.Plan{ID: "p", Steps: steps})
	if !strings.Contains(text, "Never use a task id for it.") {
		t.Errorf("approved text: %s", text)
	}
}

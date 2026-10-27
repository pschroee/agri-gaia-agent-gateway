// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// B26 (demo of 27.10.2026, evening): the agent sent create_training without the export_config of the plan step; the
// refusal names exactly what differs, one level down in nested objects, and never free text.
func TestBodyDiffNamesFields(t *testing.T) {
	st := store.PlanStep{Method: "POST", Path: "/train/config", Body: json.RawMessage(
		`{"dataset_id":"{{step2.id}}","export_config":{"opset_version":18},"train_config":{"epochs":20,"img-size":64}}`)}
	call := platform.Request{Method: "POST", Path: "/train/config", Body: []byte(
		`{"dataset_id":227,"export_config":null,"train_config":{"epochs":20,"batch-size":32}}`)}
	resolve := func(n int) (string, bool) { return "227", n == 2 }
	got := strings.Join(BodyDiff(st, call, resolve), "; ")
	for _, want := range []string{
		`export_config (the step has {"opset_version":18}, the call sends null)`,
		`train_config.batch-size (the call adds 32, the step has none)`,
		`train_config.img-size (the step has 64, the call leaves it out)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "dataset_id") || strings.Contains(got, "epochs") {
		t.Errorf("equal fields named: %q", got)
	}
	// The agent's note is free text (B23) and no difference.
	st2 := store.PlanStep{Method: "POST", Path: platform.SearchPrefix + "/s4761c58692d0/end", Body: json.RawMessage(`{"note":"a"}`)}
	call2 := platform.Request{Method: "POST", Path: platform.SearchPrefix + "/s4761c58692d0/end", Body: []byte(`{"note":"b"}`)}
	if d := BodyDiff(st2, call2, resolve); len(d) != 0 {
		t.Errorf("free text named: %v", d)
	}
}

// Only a call that leaves out fields of the step is refused at once; a changed or an added value stays a change the
// user decides on (TestPlanChangedCallNeedsApproval).
func TestOnlyLeftOut(t *testing.T) {
	st := store.PlanStep{Method: "POST", Path: "/train/config", Body: json.RawMessage(
		`{"dataset_id":5,"export_config":{"opset_version":18},"train_config":{"epochs":20,"img-size":64}}`)}
	none := func(int) (string, bool) { return "", false }
	call := func(body string) platform.Request {
		return platform.Request{Method: "POST", Path: "/train/config", Body: []byte(body)}
	}
	if d, ok := OnlyLeftOut(st, call(`{"dataset_id":5,"export_config":null,"train_config":{"epochs":20,"img-size":64}}`), none); !ok || len(d) != 1 {
		t.Fatalf("export_config null: %v %v", d, ok)
	}
	if d, ok := OnlyLeftOut(st, call(`{"dataset_id":5,"export_config":{"opset_version":18},"train_config":{"epochs":20}}`), none); !ok || len(d) != 1 {
		t.Fatalf("img-size left out: %v %v", d, ok)
	}
	for _, b := range []string{
		`{"dataset_id":5,"export_config":{"opset_version":18},"train_config":{"epochs":30,"img-size":64}}`,
		`{"dataset_id":6,"export_config":null,"train_config":{"epochs":20,"img-size":64}}`,
		`{"dataset_id":5,"export_config":{"opset_version":18},"train_config":{"epochs":20,"img-size":64,"x":1}}`,
		`{"dataset_id":5,"export_config":{"opset_version":18},"train_config":{"epochs":20,"img-size":64}}`,
	} {
		if d, ok := OnlyLeftOut(st, call(b), none); ok {
			t.Errorf("%s counted as left out: %v", b, d)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// B23: free text the agent writes for the user (descriptions, notes, titles) is no part of the plan match – the
// plan said "Laeufe", the call "Läufe". Every field that decides rights, the target object or compute stays exact.

const ftSHA = "4444444444444444444444444444444444444444444444444444444444444444"

// ftStep builds one plan step from a tool and its arguments, pinning the files as the socket does.
func ftStep(t *testing.T, tool, args string, files int) store.PlanStep {
	t.Helper()
	pins := ""
	if files > 0 {
		pins = `,"file_sha256":["` + ftSHA + `"]`
		for i := 1; i < files; i++ {
			pins = pins[:len(pins)-1] + `,"` + ftSHA + `"]`
		}
	}
	steps := build(t, `{"title":"t","steps":[{"tool":"`+tool+`","args":`+args+pins+`}]}`)
	return steps[0]
}

// ftCall builds the live call of a tool with uploads that carry the pinned hash.
func ftCall(t *testing.T, tool, args string) platform.Request {
	t.Helper()
	tl, ok := platform.Lookup(tool)
	if !ok {
		t.Fatalf("no tool %s", tool)
	}
	r, err := tl.Build(json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Files {
		r.Uploads = append(r.Uploads, platform.Upload{Field: f.Field, Name: f.Path, SHA256: ftSHA})
	}
	return r
}

func TestPlanMatchIgnoresFreeText(t *testing.T) {
	none := resolveTo("1")
	cases := []struct {
		name, tool, step, same, other string
		files                         int
	}{
		{"upload_dataset description", "upload_dataset",
			`{"name":"mnist-laeufe","description":"Bilder fuer drei Laeufe","files":["/workspace/a.png"],"annotation_labels":["0","1"]}`,
			`{"name":"mnist-laeufe","description":"Bilder für drei Läufe","files":["/workspace/a.png"],"annotation_labels":["0","1"]}`,
			`{"name":"mnist-läufe","description":"Bilder für drei Läufe","files":["/workspace/a.png"],"annotation_labels":["0","1"]}`, 1},
		{"upload_dataset labels stay exact", "upload_dataset",
			`{"name":"d","description":"x","files":["/workspace/a.png"],"annotation_labels":["0","1"]}`,
			`{"name":"d","description":"y","files":["/workspace/a.png"],"annotation_labels":["0","1"]}`,
			`{"name":"d","description":"y","files":["/workspace/a.png"],"annotation_labels":["0","2"]}`, 1},
		{"upload_model description", "upload_model",
			`{"name":"m","description":"Modell fuer Schweine","format":"onnx","model_file":"/workspace/m.onnx"}`,
			`{"name":"m","description":"Modell für Schweine","format":"onnx","model_file":"/workspace/m.onnx"}`,
			`{"name":"m","description":"Modell für Schweine","format":"pytorch","model_file":"/workspace/m.onnx"}`, 1},
		{"notebook_run note", "notebook_run",
			`{"files":["/workspace/train.py"],"entry":"train.py","dataset_ids":["17"],"note":"Laeufe"}`,
			`{"files":["/workspace/train.py"],"entry":"train.py","dataset_ids":["17"],"note":"Läufe"}`,
			`{"files":["/workspace/train.py"],"entry":"train.py","dataset_ids":["18"],"note":"Läufe"}`, 1},
		{"search_next note", "search_next",
			`{"search_id":"s4761c58692d0","params":{"lr":0.01},"note":"kleinere Lernrate fuer Lauf 2"}`,
			`{"search_id":"s4761c58692d0","params":{"lr":0.01},"note":"kleinere Lernrate für Lauf 2"}`,
			`{"search_id":"s4761c58692d0","params":{"lr":0.02},"note":"kleinere Lernrate für Lauf 2"}`, 0},
		{"end_search note", "end_search",
			`{"search_id":"s4761c58692d0","note":"Ziel erreicht, keine weiteren Laeufe"}`,
			`{"search_id":"s4761c58692d0","note":"Ziel erreicht, keine weiteren Läufe"}`,
			`{"search_id":"s4761c58692d1","note":"Ziel erreicht, keine weiteren Läufe"}`, 0},
	}
	for _, c := range cases {
		st := ftStep(t, c.tool, c.step, c.files)
		if ok, why := Matches(st, ftCall(t, c.tool, c.same), none); !ok {
			t.Errorf("%s: other free text made the call differ (%s)", c.name, why)
		}
		if ok, _ := Matches(st, ftCall(t, c.tool, c.other), none); ok {
			t.Errorf("%s: a field that decides rights, target or compute was ignored", c.name)
		}
	}
}

// A free-text field the step has and the call leaves out (or the other way round) is free text too; a field that
// is not on the list never is, even when it holds text.
func TestPlanMatchFreeTextOnlyListed(t *testing.T) {
	none := resolveTo("1")
	st := ftStep(t, "end_search", `{"search_id":"s4761c58692d0","note":"genug"}`, 0)
	if ok, why := Matches(st, ftCall(t, "end_search", `{"search_id":"s4761c58692d0"}`), none); !ok {
		t.Fatalf("a dropped note made the call differ: %s", why)
	}
	// update_model's name is free to read but names the object a build addresses: it stays exact.
	ren := store.PlanStep{Method: "PATCH", Path: "/models/42", Body: json.RawMessage(`{"name":"Modell fuer Schweine"}`)}
	if ok, _ := Matches(ren, platform.Request{Method: "PATCH", Path: "/models/42", Body: []byte(`{"name":"Modell für Schweine"}`)}, none); ok {
		t.Fatal("the name of a model was treated as free text")
	}
	// A note in the body of a path that is not on the list stays exact.
	raw := store.PlanStep{Method: "POST", Path: "/train/config", Body: json.RawMessage(`{"note":"a"}`)}
	if ok, _ := Matches(raw, platform.Request{Method: "POST", Path: "/train/config", Body: []byte(`{"note":"b"}`)}, none); ok {
		t.Fatal("a note outside the list was ignored")
	}
}

// Every listed free-text field is a parameter of its tool, so the list cannot silently go stale.
func TestPlanFreeTextFieldsAreToolParams(t *testing.T) {
	for _, r := range freeTextRules {
		tl, ok := platform.Lookup(r.tool)
		if !ok {
			t.Errorf("rule for unknown tool %s", r.tool)
			continue
		}
		have := map[string]bool{}
		for _, p := range tl.Params {
			have[p.Name] = true
		}
		for _, f := range r.fields {
			if !have[f] {
				t.Errorf("%s has no parameter %s", r.tool, f)
			}
		}
	}
}

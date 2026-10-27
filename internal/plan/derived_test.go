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

// splitStep is a raw step for the split path; body is the stored body.
func splitStep(body string) store.PlanStep {
	return store.PlanStep{Idx: 2, Method: "POST", Path: platform.AssessSplitPath, Body: json.RawMessage(body)}
}

func splitCall(t *testing.T, body string) platform.Request {
	t.Helper()
	return live(t, "POST", platform.AssessSplitPath, body)
}

func TestWithDerivedDropsDefaultName(t *testing.T) {
	st := splitStep(`{"dataset_id":"{{step1.id}}","train_share":70}`)
	call := splitCall(t, `{"dataset_id":185,"name":"demo-train","train_share":70}`)
	if same, _ := Matches(st, call, resolveTo("185")); same {
		t.Fatal("the call must differ from the step before the loosening")
	}
	out, ok := WithDerived(st, call, resolveTo("185"), "name", "demo-train")
	if !ok {
		t.Fatal("not accepted")
	}
	var got map[string]any
	if err := json.Unmarshal(out.Body, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["name"]; has {
		t.Fatalf("name must be dropped: %s", out.Body)
	}
	if same, why := Matches(st, out, resolveTo("185")); !same {
		t.Fatalf("returned call does not match the step: %s", why)
	}
	// The sent body of the agent is untouched.
	if string(call.Body) == string(out.Body) {
		t.Fatal("the call was not changed")
	}
}

func TestWithDerivedAddsDefaultName(t *testing.T) {
	st := splitStep(`{"dataset_id":185,"name":"demo-train","train_share":70}`)
	call := splitCall(t, `{"dataset_id":185,"train_share":70}`)
	out, ok := WithDerived(st, call, resolveTo("185"), "name", "demo-train")
	if !ok {
		t.Fatal("not accepted")
	}
	var got map[string]any
	if err := json.Unmarshal(out.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "demo-train" {
		t.Fatalf("name missing: %s", out.Body)
	}
	if same, why := Matches(st, out, resolveTo("185")); !same {
		t.Fatalf("returned call does not match the step: %s", why)
	}
}

func TestWithDerivedRefuses(t *testing.T) {
	r := resolveTo("185")
	cases := map[string]struct {
		step, call, derived string
		resolve             Resolve
	}{
		"other name":        {`{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"name":"other","train_share":70}`, "demo-train", r},
		"name not a string": {`{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"name":5,"train_share":70}`, "5", r},
		"empty derived":     {`{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"name":"","train_share":70}`, "", r},
		"different names":   {`{"dataset_id":185,"name":"a","train_share":70}`, `{"dataset_id":185,"name":"demo-train","train_share":70}`, "demo-train", r},
		"other train_share": {`{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"name":"demo-train","train_share":80}`, "demo-train", r},
		"unresolved":        {`{"dataset_id":"{{step1.id}}","train_share":70}`, `{"dataset_id":185,"name":"demo-train","train_share":70}`, "demo-train", resolveTo("")},
		"extra field":       {`{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"name":"demo-train","train_share":70,"x":1}`, "demo-train", r},
		"name in neither":   {`{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"train_share":70}`, "demo-train", r},
		"name in both":      {`{"dataset_id":185,"name":"demo-train","train_share":70}`, `{"dataset_id":185,"name":"demo-train","train_share":70}`, "demo-train", r},
	}
	for name, c := range cases {
		call := splitCall(t, c.call)
		out, ok := WithDerived(splitStep(c.step), call, c.resolve, "name", c.derived)
		if ok {
			t.Errorf("%s: accepted", name)
		}
		if string(out.Body) != string(call.Body) {
			t.Errorf("%s: call changed: %s", name, out.Body)
		}
	}
}

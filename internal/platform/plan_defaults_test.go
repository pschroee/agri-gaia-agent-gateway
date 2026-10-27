// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

// #236: a split with and without the default share is the same call.
func TestSplitDatasetWritesDefaultShare(t *testing.T) {
	split, ok := Lookup("split_dataset")
	if !ok {
		t.Fatal("split_dataset missing")
	}
	a, err := split.Build(json.RawMessage(`{"dataset_id":5}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := split.Build(json.RawMessage(`{"dataset_id":5,"train_share":70}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Body) != string(b.Body) || string(a.Body) != `{"dataset_id":5,"train_share":70}` {
		t.Fatalf("bodies differ: %s vs %s", a.Body, b.Body)
	}
}

// #236: a guard against future outliers. For each tool whose default the gateway writes out, the minimal call and the
// call with the explicit default build the same request.
func TestPlanToolsStaticDefaultsSameCall(t *testing.T) {
	cases := []struct {
		tool, minimal, explicit string
	}{
		{"pre_annotate", `{"dataset_id":5,"function":"f","mapping":{"a":"b"}}`, `{"dataset_id":5,"function":"f","mapping":{"a":"b"},"min_confidence":50,"existing":"keep"}`},
		{"annotation_rollback", `{"source_id":"run-1"}`, `{"source_id":"run-1","scope":"all"}`},
		{"assess_model", `{"model_id":3}`, `{"model_id":3,"final":false}`},
		{"split_dataset", `{"dataset_id":5}`, `{"dataset_id":5,"train_share":70}`},
	}
	for _, c := range cases {
		tool, ok := Lookup(c.tool)
		if !ok {
			t.Errorf("%s: tool missing", c.tool)
			continue
		}
		a, errA := tool.Build(json.RawMessage(c.minimal))
		b, errB := tool.Build(json.RawMessage(c.explicit))
		if errA != nil || errB != nil {
			t.Errorf("%s: build failed: %v / %v", c.tool, errA, errB)
			continue
		}
		if a.Method != b.Method || a.Path != b.Path || string(a.Body) != string(b.Body) {
			t.Errorf("%s: minimal %s %s %s, explicit %s %s %s", c.tool, a.Method, a.Path, a.Body, b.Method, b.Path, b.Body)
		}
	}
}

// #236: handler, preview and plan match use one rule for the default name.
func TestSplitDefaultNameOneRule(t *testing.T) {
	if got := SplitDefaultName("demo-pruefung"); got != "demo-pruefung-train" {
		t.Fatalf("default name %q", got)
	}
	if !strings.HasSuffix(SplitDefaultName("x"), "-train") {
		t.Fatal("suffix")
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// The steps of the correction loop of a pre-annotation (V3c) are steps of a plan like any other call: the
// classification of the gateway (delegation.Classify) decides what the approval of the plan covers. Taking
// the automatic annotations back (write) and filling again with the corrected setting (compute) are
// covered, so a round may run inside an approved plan without asking again; building a function is
// irreversible and never covered, so the user approves exactly that call on its own (E9).
func TestCorrectionStepsCoveredInPlan(t *testing.T) {
	// The classification of the gateway, the one planClassify uses (issue #63), not the fallback.
	gateway := func(r platform.Request) (string, string, bool) {
		a := delegation.Classify(r)
		return string(a.Effect), a.Action, a.ConfirmEach
	}
	raw := `{"title":"Correct the pre-annotation","steps":[
	 {"method":"POST","path":"/_agw/annotation/rollbacks","body":{"source_id":"run-1","scope":"all"},"description":"take the automatic annotations back"},
	 {"method":"POST","path":"/_agw/annotation/fills","body":{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"class_0":"0"}},"description":"fill again with the corrected mapping"},
	 {"method":"POST","path":"/_agw/annotation/functions","body":{"model_id":9,"kind":"classification","labels":["class_0","class_1"]},"description":"build a function from the new model"}]}`
	steps, err := Build(proposal(t, raw), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps: %+v", steps)
	}
	if steps[0].Effect != "write" || !steps[0].Covered {
		t.Errorf("the rollback is not a covered write: %+v", steps[0])
	}
	if steps[1].Effect != "compute" || !steps[1].Covered {
		t.Errorf("the fill is not a covered compute step: %+v", steps[1])
	}
	if steps[2].Effect != "irreversible" || steps[2].Covered {
		t.Errorf("building a function must not be covered by the plan: %+v", steps[2])
	}
	// A run over all images is a correction round with the same class as a fill: compute, covered.
	run := `{"title":"Correct with a run","steps":[
	 {"method":"POST","path":"/_agw/annotation/runs","body":{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"class_0":"0"},"existing":"keep"}}]}`
	steps, err = Build(proposal(t, run), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Effect != "compute" || !steps[0].Covered {
		t.Errorf("the run is not a covered compute step: %+v", steps[0])
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// autoGrant builds the grant of an autonomous plan from the given steps.
func autoGrant(t *testing.T, steps ...store.PlanStep) delegation.Delegation {
	t.Helper()
	for i := range steps {
		steps[i].Idx = i + 1
	}
	g, err := Grant(steps, SampleStep)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func hasRule(g delegation.Delegation, action, resource string, ids ...string) bool {
	for _, r := range g.Rules {
		if r.Action != action || r.Resource != resource {
			continue
		}
		if len(ids) == 0 {
			return len(r.IDs) == 0
		}
		if len(r.IDs) == len(ids) {
			ok := true
			for i := range ids {
				if r.IDs[i] != ids[i] {
					ok = false
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// The delegation of an autonomous plan comes from its steps (A-E1, D1.2): a create without an id, a
// placeholder as "own", a fixed id as itself, and reading open.
func TestGrantFromSteps(t *testing.T) {
	g := autoGrant(t,
		store.PlanStep{Method: "POST", Path: "/datasets", Effect: EffectWrite},
		store.PlanStep{Method: "POST", Path: "/train/config", Body: json.RawMessage(`{"dataset_id":1}`), Effect: EffectCompute},
		store.PlanStep{Method: "POST", Path: "/train/containers/{{step2.id}}/run", Effect: EffectCompute},
		store.PlanStep{Method: "GET", Path: "/train/containers/{{step2.id}}/model", Effect: EffectWrite},
	)
	for _, want := range []struct{ action, resource string }{
		{delegation.Create, delegation.Dataset},
		{delegation.Create, delegation.Training},
		{delegation.Run, delegation.Training},
		{delegation.Create, delegation.Model},
	} {
		if !hasRule(g, want.action, want.resource) {
			t.Errorf("grant misses %s %s without an id: %+v", want.action, want.resource, g.Rules)
		}
	}
	if !hasRule(g, delegation.Run, delegation.Training, delegation.Own) {
		t.Errorf("a placeholder must become own: %+v", g.Rules)
	}
	// The reads are open for every resource.
	for _, res := range delegation.Resources() {
		if !hasRule(g, delegation.Read, res, delegation.All) {
			t.Errorf("grant misses read %s *", res)
		}
	}
	if !g.Enforcing() {
		t.Errorf("a plan's grant must enforce")
	}
}

// A step on a path the routes do not know becomes a rule with the exact path only; it never covers another
// path (A-E1, security question).
func TestGrantNeverIncludesUnlistedWrites(t *testing.T) {
	g := autoGrant(t, store.PlanStep{Method: "PUT", Path: "/deploy/4", Effect: EffectWrite})
	now := time.Now()
	own := func(string, string) bool { return false }
	inside := platform.Request{Method: "PUT", Path: "/deploy/4"}
	if dec := g.Check(delegation.Classify(inside), now, own); !dec.Allowed {
		t.Fatalf("the step's own call must be covered: %s", dec.Reason)
	}
	outside := platform.Request{Method: "PUT", Path: "/deploy/5"}
	if dec := g.Check(delegation.Classify(outside), now, own); dec.Allowed {
		t.Fatalf("another path must not be covered by the step's rule")
	}
	// A dataset write without a rule of its own is refused as well.
	if dec := g.Check(delegation.Classify(platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"x"}`)}), now, own); dec.Allowed {
		t.Fatalf("an unlisted write must be refused")
	}
}

// Reading stays open while a plan runs (D1.3); a writing GET stays a write and is not covered by it.
func TestGrantReadRules(t *testing.T) {
	g := autoGrant(t, store.PlanStep{Method: "POST", Path: "/datasets", Effect: EffectWrite})
	now := time.Now()
	own := func(string, string) bool { return false }
	for _, path := range []string{"/datasets", "/datasets/3", "/models/5", "/train/containers/7/status", "/tasks/40", "/agrovoc/keywords"} {
		if dec := g.Check(delegation.Classify(platform.Request{Method: "GET", Path: path}), now, own); !dec.Allowed {
			t.Errorf("read %s must be allowed: %s", path, dec.Reason)
		}
	}
	// A writing GET (creates a model in the backend) is a write and needs a rule.
	if dec := g.Check(delegation.Classify(platform.Request{Method: "GET", Path: "/train/containers/7/model"}), now, own); dec.Allowed {
		t.Errorf("a writing GET must not be covered by the read rules")
	}
}

// The rules are deduplicated and sorted, so the preview is stable.
func TestGrantRulesStable(t *testing.T) {
	a := autoGrant(t,
		store.PlanStep{Method: "POST", Path: "/datasets"},
		store.PlanStep{Method: "POST", Path: "/datasets"},
	)
	b := autoGrant(t,
		store.PlanStep{Method: "POST", Path: "/datasets"},
		store.PlanStep{Method: "POST", Path: "/datasets"},
	)
	ka, _ := json.Marshal(a.Rules)
	kb, _ := json.Marshal(b.Rules)
	if string(ka) != string(kb) {
		t.Fatalf("rules differ: %s vs %s", ka, kb)
	}
	n := 0
	for _, r := range a.Rules {
		if r.Action == delegation.Create && r.Resource == delegation.Dataset {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate rule kept: %+v", a.Rules)
	}
}

// ComputeSteps sums the compute steps times their max_attempts (D1.7).
func TestComputeSteps(t *testing.T) {
	steps := []store.PlanStep{
		{Effect: EffectWrite, MaxAttempts: 1},
		{Effect: EffectCompute, MaxAttempts: 1},
		{Effect: EffectCompute, MaxAttempts: 3},
	}
	if got := ComputeSteps(steps); got != 4 {
		t.Fatalf("compute steps: %d, want 4", got)
	}
}

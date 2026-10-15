// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"testing"
	"time"

	"agw/internal/platform"
)

// A refinement of a route runs right after its classification and reaches Also, ID, Effect and
// Problem (G1.6); routes without one keep their classification.
func TestRefineRuns(t *testing.T) {
	routes = append(routes, r("POST", `/seam/`+seg, Create, Dataset, 1).refine(func(a *Access, req platform.Request) {
		a.Also = append(a.Also, Access{Action: Read, Resource: Training, ID: "7"})
		a.Effect = EffectCompute
		a.Problem = "refined: " + req.Path
	}))
	t.Cleanup(func() { routes = routes[:len(routes)-1] })

	got := Classify(platform.Request{Method: "POST", Path: "/seam/12"})
	if got.Action != Create || got.Resource != Dataset || got.ID != "12" {
		t.Errorf("classification lost: %+v", got)
	}
	if got.Effect != EffectCompute {
		t.Errorf("refined effect not taken over: %q", got.Effect)
	}
	if got.Problem != "refined: /seam/12" {
		t.Errorf("refined problem: %q", got.Problem)
	}
	if len(got.Also) != 1 || got.Also[0].Resource != Training || got.Also[0].ID != "7" {
		t.Errorf("refined Also: %+v", got.Also)
	}
	// A route without a refinement is unchanged: the class comes from its action.
	if other := Classify(platform.Request{Method: "POST", Path: "/datasets"}); other.Effect != EffectWrite || other.Problem != "" || other.ConfirmEach {
		t.Errorf("other route changed: %+v", other)
	}
	// The refinement only runs for its own route.
	if other := Classify(platform.Request{Method: "GET", Path: "/datasets"}); other.Effect != EffectRead {
		t.Errorf("GET /datasets: %+v", other)
	}
}

// A route marked confirm-each carries the trait in its classified access (G1.1).
func TestRouteConfirmEach(t *testing.T) {
	routes = append(routes, r("PUT", `/seam-deploy/`+seg, Update, API, 1).confirmEach())
	t.Cleanup(func() { routes = routes[:len(routes)-1] })

	got := Classify(platform.Request{Method: "PUT", Path: "/seam-deploy/4"})
	if !got.ConfirmEach {
		t.Errorf("confirm each missing: %+v", got)
	}
	if got.Effect != EffectWrite {
		t.Errorf("class changed: %q", got.Effect)
	}
	if other := Classify(platform.Request{Method: "GET", Path: "/datasets"}); other.ConfirmEach {
		t.Errorf("other route asks every call: %+v", other)
	}
}

// Cases a feature registered run with the base table, which keeps its order in front (G1.6).
func TestRegisteredConformanceCasesRun(t *testing.T) {
	registerConformance(
		Case{Name: "seam: reading models is not delegated", Req: platform.Request{Method: "GET", Path: "/models"}, Group: "seam"},
		Case{Name: "seam: reading datasets is delegated", Req: platform.Request{Method: "GET", Path: "/datasets"}, Allowed: true, Group: "seam"},
	)
	t.Cleanup(func() { registeredCases = registeredCases[:len(registeredCases)-2] })

	cases := ConformanceCases()
	if len(cases) < 2 {
		t.Fatal("no cases")
	}
	base := baseCases()
	for i, c := range base {
		if cases[i].Name != c.Name {
			t.Fatalf("base case %d moved: %q instead of %q", i, cases[i].Name, c.Name)
		}
	}
	for _, c := range cases[len(base):] {
		allowed, why := Evaluate(c, time.Now())
		if allowed != c.Allowed {
			t.Errorf("%s: allowed=%v, expected %v (%s)", c.Name, allowed, c.Allowed, why)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"testing"
	"time"

	"agw/internal/platform"
)

// The route of the deployment check (R3.1): the call reads the deployment of this chat and the model in
// the query, and its effect class is read — it changes nothing, asks no approval and counts against no
// compute limit.
func TestDeploymentCheckRoute(t *testing.T) {
	req := platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/deployments/9",
		Query: map[string]string{"model_id": "5"}}
	got := Classify(req)
	if got.Action != Run || got.Resource != ContainerDeployment || got.ID != "9" {
		t.Errorf("classification: %+v", got)
	}
	if got.Effect != EffectRead {
		t.Errorf("effect: %q", got.Effect)
	}
	if got.ConfirmEach {
		t.Error("the check asks for every call")
	}
	if len(got.Also) != 1 || got.Also[0].Action != Read || got.Also[0].Resource != Model || got.Also[0].ID != "5" {
		t.Errorf("the model the check reads: %+v", got.Also)
	}
	// A call without a model names none: no rule covers that, so the delegation refuses it.
	bare := Classify(platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/deployments/9"})
	if len(bare.Also) != 1 || bare.Also[0].ID != unknownID {
		t.Errorf("without model_id: %+v", bare.Also)
	}
	// Another path of the assessment prefix keeps its own class (the check status).
	status := Classify(platform.Request{Method: http.MethodGet,
		Path: platform.AssessPrefix + "/checks/11111111-2222-3333-4444-555555555555"})
	if status.Action != Read || status.Effect != EffectRead || len(status.Also) != 0 {
		t.Errorf("check status: %+v", status)
	}
}

// The conformance cases of this feature run with the others (G1.6): each of them has to be refused or
// let through as it says, and the control case proves that a delegation which allows the call really
// lets it through.
func TestAssessEdgeConformanceCases(t *testing.T) {
	cases := ConformanceCases()
	control, others := 0, 0
	for _, c := range cases {
		if c.Group != "control" && c.Group != "resource" && c.Group != "object" {
			continue
		}
		if c.Req.Path != platform.AssessPrefix+"/deployments/9" {
			continue
		}
		if c.Allowed {
			control++
		} else {
			others++
		}
		allowed, why := Evaluate(c, time.Now())
		if allowed != c.Allowed {
			t.Errorf("%s: allowed=%v, expected %v (%s)", c.Name, allowed, c.Allowed, why)
		}
	}
	if control != 1 || others != 3 {
		t.Errorf("control cases %d, refused cases %d", control, others)
	}
}

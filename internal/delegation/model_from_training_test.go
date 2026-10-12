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

// modelRules allows creating models and reading training 7, nothing else about trainings.
const modelRules = `{"rules": [
  {"action": "create", "resource": "model"},
  {"action": "read",   "resource": "training", "ids": ["7"]}
]}`

func TestModelFromTrainingConformance(t *testing.T) {
	a := Classify(platform.Request{Method: http.MethodGet, Path: "/train/containers/7/model"})
	if a.String() != "create model + read training 7" {
		t.Fatalf("classified as %q", a.String())
	}
	cases := []Case{
		{Name: "own training", Tool: "model-from-training", Args: `{"train_container_id":7}`, Rules: modelRules, Allowed: true},
		{Name: "padded id", Tool: "model-from-training", Args: `{"train_container_id":"07"}`, Rules: modelRules, Allowed: true},
		{Name: "foreign training", Tool: "model-from-training", Args: `{"train_container_id":8}`, Rules: modelRules, Allowed: false},
		{Name: "no create model", Tool: "model-from-training", Args: `{"train_container_id":7}`,
			Rules: `{"rules": [{"action": "read", "resource": "training", "ids": ["7"]}]}`, Allowed: false},
	}
	for _, c := range cases {
		got, why := Evaluate(c, time.Now())
		if got != c.Allowed {
			t.Errorf("%s: allowed=%v, expected %v (%s)", c.Name, got, c.Allowed, why)
		}
	}
}

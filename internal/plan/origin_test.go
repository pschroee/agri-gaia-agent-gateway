// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"testing"

	"agw/internal/store"
)

func TestDeploymentImage(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		ref     int
		literal string
	}{
		{"placeholder", `{"container_image_id":"{{step2.id}}"}`, 2, ""},
		{"number", `{"container_image_id":87}`, 0, "87"},
		{"digit string", `{"container_image_id":"87"}`, 0, "87"},
		{"zero", `{"container_image_id":0}`, 0, ""},
		{"zero string", `{"container_image_id":"0"}`, 0, ""},
		{"absent", `{"name":"x"}`, 0, ""},
		{"empty body", ``, 0, ""},
		{"not json", `nope`, 0, ""},
		{"text around placeholder", `{"container_image_id":"a{{step2.id}}"}`, 0, ""},
		{"null", `{"container_image_id":null}`, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, lit := DeploymentImage(store.PlanStep{Body: []byte(c.body)})
			if ref != c.ref || lit != c.literal {
				t.Fatalf("got (%d, %q), want (%d, %q)", ref, lit, c.ref, c.literal)
			}
		})
	}
}

func TestStepKindHelpers(t *testing.T) {
	build := store.PlanStep{Method: "POST", Path: "/container-images/build"}
	create := store.PlanStep{Method: "POST", Path: "/container-deployments"}
	deploy := store.PlanStep{Method: "PUT", Path: "/container-deployments/{{step2.id}}/deploy"}
	if !IsBuildStep(build) || IsBuildStep(create) || IsBuildStep(deploy) {
		t.Fatal("IsBuildStep")
	}
	if !IsCreateDeploymentStep(create) || IsCreateDeploymentStep(build) || IsCreateDeploymentStep(deploy) {
		t.Fatal("IsCreateDeploymentStep")
	}
	if !IsDeployStep(deploy) || IsDeployStep(create) || IsDeployStep(store.PlanStep{Method: "PUT", Path: "/container-deployments/5/undeploy"}) {
		t.Fatal("IsDeployStep")
	}
}

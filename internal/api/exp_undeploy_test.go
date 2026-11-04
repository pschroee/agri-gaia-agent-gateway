// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import "testing"

// The runner's cleanup may PUT only the undeploy of a deployment; every other path stays refused.
func TestExpUndeployPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/container-deployments/32/undeploy":    true,
		"/container-deployments/32/deploy":      false,
		"/container-deployments/x/undeploy":     false,
		"/container-deployments/32/undeploy/..": false,
		"/models/32/undeploy":                   false,
		"/container-deployments/32":             false,
	} {
		if got := expUndeployPath.MatchString(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}

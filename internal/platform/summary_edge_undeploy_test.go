// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

// Taking a deployment back names the deployment, its id and the device, and says what stays: the
// deployment itself, so it can be started again (edge D3.1, D3.3).
func TestSummaryUndeploy(t *testing.T) {
	got := summarize(t, edgeSummaryReads(), SummaryCall{Tool: "undeploy", Method: "PUT", Path: "/container-deployments/9/undeploy"})
	wantLines(t, got,
		`Undeploy "agw-mnist" (id 9) from edge device "sim-edge-01" (id 1)`,
		"The container and its anonymous volumes are removed; the deployment can be started again.")
	if strings.Contains(got, "Also sends") {
		t.Fatalf("the summary hides or invents a field:\n%s", got)
	}
}

// Deleting a deployment names it and says that nothing brings it back (edge D-E3).
func TestSummaryDeleteDeployment(t *testing.T) {
	got := summarize(t, edgeSummaryReads(), SummaryCall{Tool: "delete_deployment", Method: "DELETE", Path: "/container-deployments/9"})
	wantLines(t, got,
		`Delete deployment "agw-mnist" (id 9)`,
		"This cannot be undone.")
	if strings.Contains(got, "Also sends") {
		t.Fatalf("the summary hides or invents a field:\n%s", got)
	}
}

// A deployment that cannot be read is still named by its id: taking it back shows the id and says that
// the name and the device are missing, instead of inventing one.
func TestSummaryUndeployWithoutReads(t *testing.T) {
	got := summarize(t, nil, SummaryCall{Tool: "undeploy", Method: "PUT", Path: "/container-deployments/9/undeploy"})
	wantLines(t, got,
		"Undeploy deployment 9",
		"The container and its anonymous volumes are removed; the deployment can be started again.")
}

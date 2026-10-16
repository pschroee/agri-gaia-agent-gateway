// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

// The reads of an edge summary: the device list, the image list and one deployment.
func edgeSummaryReads() *fakeReads {
	return &fakeReads{answers: map[string]string{
		"/edge-devices":            `[{"id":1,"name":"sim-edge-01","arch":"amd64","registered":true,"last_heartbeat":"2026-10-16T10:00:00.000000"}]`,
		"/container-images":        `[{"id":5,"repository":"agent-test/mnist-triton","tag":"v1"}]`,
		"/container-deployments/9": `{"id":9,"name":"agw-mnist","edge_device_id":1,"container_image_id":5}`,
	}}
}

// The summary of a new deployment names the image, the device and every port, and it says that
// nothing runs yet (edge D2.7). Nothing of the body stays under "Also sends".
func TestSummaryCreateDeployment(t *testing.T) {
	f := edgeSummaryReads()
	got := summarize(t, f, SummaryCall{Tool: "create_deployment", Method: "POST", Path: "/container-deployments",
		Body: json.RawMessage(`{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist",` +
			`"port_bindings":[{"host_port":"18100","container_port":"8000","protocol":"tcp"},{"host_port":"18101","container_port":"8001","protocol":"tcp"}]}`)})
	wantLines(t, got,
		`Create deployment "agw-mnist" of "agent-test/mnist-triton:v1" on edge device "sim-edge-01" (id 1)`,
		"Ports: 18100->8000/tcp, 18101->8001/tcp",
		"Nothing runs until the deployment is started.")
	if strings.Contains(got, "Also sends") {
		t.Fatalf("a named field stays hidden or unnamed fields leak:\n%s", got)
	}
}

// A plan step of create_deployment without image_id carries the placeholder 0: the summary says the
// image is resolved when the step runs instead of naming the wrong one (edge D2.5).
func TestSummaryCreateDeploymentWithoutImageID(t *testing.T) {
	got := summarize(t, edgeSummaryReads(), SummaryCall{Tool: "create_deployment", Method: "POST", Path: "/container-deployments",
		Body: json.RawMessage(`{"edge_device_id":1,"container_image_id":0,"name":"agw-mnist",` +
			`"port_bindings":[{"host_port":"18100","container_port":"8000","protocol":"tcp"}]}`)})
	wantLines(t, got,
		`Create deployment "agw-mnist" of an image the tool resolves when the step runs on edge device "sim-edge-01" (id 1)`,
		"Ports: 18100->8000/tcp")
}

// The summary of a start names the deployment, its image and says that the container runs with network
// access and opens its ports on every interface of the device (edge D2.7).
func TestSummaryStartDeployment(t *testing.T) {
	f := edgeSummaryReads()
	got := summarize(t, f, SummaryCall{Tool: "start_deployment", Method: "PUT", Path: "/container-deployments/9/deploy"})
	wantLines(t, got,
		`Start deployment "agw-mnist" (id 9) on edge device "sim-edge-01" (id 1)`,
		`Image: "agent-test/mnist-triton:v1"`,
		"The device pulls the image and runs it with network access; ports are opened on all interfaces of the device.")
	if strings.Contains(got, "Also sends") {
		t.Fatalf("the summary hides or invents a field:\n%s", got)
	}
}

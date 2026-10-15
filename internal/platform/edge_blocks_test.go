// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Every path through which the agent could bring code onto an edge device or change the devices
// themselves is blocked in Normalize, so for the tool, the raw request and the REST binding alike and
// whatever the delegation says (edge D1.1).
func TestBlockedEdgeCalls(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodPost, "/applications", MsgApplicationsBlocked},
		{http.MethodPut, "/applications/3", MsgApplicationsBlocked},
		{http.MethodDelete, "/applications/3", MsgApplicationsBlocked},
		{http.MethodGet, "/applications", MsgApplicationsBlocked},
		{http.MethodGet, "/Applications", MsgApplicationsBlocked}, // compared in lower case
		{http.MethodPost, "/model-deployments", MsgModelDeployBlocked},
		{http.MethodGet, "/model-deployments", MsgModelDeployBlocked},
		{http.MethodPut, "/model-deployments/2/deploy", MsgModelDeployBlocked},
		{http.MethodPost, "/tags", MsgEdgeReadOnly},
		{http.MethodDelete, "/tags/3", MsgEdgeReadOnly},
		{http.MethodPost, "/edge-groups", MsgEdgeReadOnly},
		{http.MethodDelete, "/edge-groups/2", MsgEdgeReadOnly},
		{http.MethodPost, "/edge-devices", MsgEdgeReadOnly},
		{http.MethodDelete, "/edge-devices/3", MsgEdgeReadOnly},
		{http.MethodGet, "/edge-devices/3", MsgEdgeReadOnly}, // the writing GET: it registers the device
		{http.MethodGet, "/Edge-Devices/3", MsgEdgeReadOnly},
		{http.MethodPost, "/edge-devices/3/deployments", MsgEdgeReadOnly},
	}
	for _, c := range cases {
		if _, err := Normalize(Request{Method: c.method, Path: c.path}); err == nil || err.Error() != c.want {
			t.Errorf("%s %s: err=%v, want %q", c.method, c.path, err, c.want)
		}
	}
}

// The readings the agent keeps: the device list, the deployments of a device, the groups and the tags
// (edge D1.1).
func TestEdgeReadsPass(t *testing.T) {
	for _, path := range []string{"/edge-devices", "/edge-groups", "/edge-groups/3", "/tags", "/Tags", "/edge-devices/3/deployments"} {
		if _, err := Normalize(Request{Method: http.MethodGet, Path: path}); err != nil {
			t.Errorf("GET %s: %v", path, err)
		}
	}
}

// A training image is recognised by its tag or by the ports it exposes (edge D1.10); nothing else
// counts as one.
func TestIsTrainingImage(t *testing.T) {
	cases := []struct {
		tag    string
		ports  []string
		train  bool
		reason string
	}{
		{"mnist-train", nil, true, "tag ends on -train"},
		{"Food101-TRAIN", nil, true, "the tag is compared in lower case"},
		{"mnist-triton", []string{"8000/tcp", "8001/tcp", "8002/tcp"}, false, "an inference image"},
		{"v1", []string{"8888/tcp"}, true, "Jupyter"},
		{"v1", []string{"6006/tcp"}, true, "TensorBoard"},
		{"v1", []string{"8888"}, true, "the port without the protocol"},
		{"v1", nil, false, "no hint"},
		{"", []string{"8000/tcp"}, false, "no tag, no training port"},
		{"mnist-triton", []string{"8000/tcp", "6006/tcp"}, true, "one training port is enough"},
	}
	for _, c := range cases {
		if got := IsTrainingImage(c.tag, c.ports); got != c.train {
			t.Errorf("IsTrainingImage(%q, %v) = %v, want %v (%s)", c.tag, c.ports, got, c.train, c.reason)
		}
	}
}

// The edge key of a device never reaches the agent, also not through a reading call that stays open
// (edge D-E1). Creating a device used to hand it out.
func TestEdgeKeyRedacted(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":3,"name":"sim-edge-01","edge_key":"EK-secret","registered":true}]`)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: http.MethodGet, Path: "/edge-devices"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.HTTPStatus != http.StatusOK {
		t.Fatalf("reading the devices: %+v", res)
	}
	if strings.Contains(res.Body, "EK-secret") {
		t.Fatalf("the edge key reached the agent: %s", res.Body)
	}
	for _, want := range []string{`"name":"sim-edge-01"`, `"edge_key":"[redacted by the orchestrator]"`} {
		if !strings.Contains(res.Body, want) {
			t.Fatalf("the answer lost %q: %s", want, res.Body)
		}
	}
}

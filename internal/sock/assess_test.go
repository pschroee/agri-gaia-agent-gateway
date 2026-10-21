// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"strings"
	"testing"
)

// The REST binding reaches the check paths only as the check tools (#199, R0): the call is built by the tool,
// so the gateway still decides what a check runs on; the reading paths of the gateway and calls without
// a tool stay closed.
func TestRESTAssessViaTools(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-ras", decide: "approved"}}
	c := start(t, b)
	code, body, _ := apiCall(t, c, "POST", "/platform-api/_agw/assess/splits", "application/json", `{"dataset_id":5,"train_share":80}`, nil)
	if code != 200 {
		t.Fatalf("split_dataset over REST: %d %s", code, body)
	}
	if len(b.reqs) != 1 || !strings.Contains(b.reqs[0], "POST /_agw/assess/splits|") || !strings.Contains(b.reqs[0], `"dataset_id":5`) {
		t.Fatalf("what reached the backend: %v", b.reqs)
	}
	for _, call := range []struct{ m, p, body string }{
		{"GET", "/platform-api/_agw/assess/checks", ""},                                  // no id: no tool
		{"POST", "/platform-api/_agw/assess/checks", `{"path":"/workspace/model.onnx"}`}, // sandbox file
		{"GET", "/platform-api/_agw/assess/models/5/latest", ""},                         // reading path of the gateway
		{"POST", "/platform-api/_agw/assess/checks", `{"model_id":4,"preprocess":{}}`},   // not available here
	} {
		code, body, _ := apiCall(t, c, call.m, call.p, "", call.body, nil)
		if code != 400 {
			t.Errorf("%s %s: %d %s", call.m, call.p, code, body)
		}
	}
	if len(b.reqs) != 1 {
		t.Errorf("a refused call reached the backend: %v", b.reqs)
	}
	refused := 0
	for _, l := range b.calls {
		if strings.HasPrefix(l, "cli:platform:refused: ") {
			refused++
		}
	}
	if refused != 4 {
		t.Errorf("refusals in the log: %d of 4 (%v)", refused, b.calls)
	}
}

// The raw tool request refuses the check paths too.
func TestRequestToolRefusesAssessPaths(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-raq", decide: "approved"}}
	c := start(t, b)
	for _, args := range []string{
		`{"method":"GET","path":"/_agw/assess/checks/11111111-2222-3333-4444-555555555555"}`,
		`{"method":"POST","path":"/_agw/assess/checks"}`,
	} {
		code, r := cliPlatform(t, c, "request", args)
		if code != 200 || r.Status != "error" || !strings.Contains(r.Message, "check calls go through the assess tools") {
			t.Errorf("%s: %d %+v", args, code, r)
		}
	}
	if len(b.reqs) != 0 {
		t.Errorf("the check paths reached the backend: %v", b.reqs)
	}
	// A path that only shares a prefix is not a check path.
	code, r := cliPlatform(t, c, "request", `{"method":"GET","path":"/datasets"}`)
	if code != 200 || r.Status != "ok" {
		t.Errorf("a normal call: %d %+v", code, r)
	}
}

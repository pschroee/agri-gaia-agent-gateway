// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"strings"
	"testing"
)

// #199: a search is proposed over REST with a JSON body; the call that reaches the backend is the one of
// the tool, with a form (the manager reads the form, not JSON).
func TestRESTGatewaySearch(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-rs", decide: "approved"}}
	c := start(t, b)
	body := `{"title":"lr","path":"template","space":{"lr":{"type":"float","min":0.0001,"max":0.1,"scale":"log"}},"base_container_id":3}`
	if code, out, _ := apiCall(t, c, "POST", "/platform-api/_agw/search", "application/json", body, nil); code != 200 {
		t.Fatalf("propose over REST: %d %s", code, out)
	}
	if code, out, _ := apiCall(t, c, "GET", "/platform-api/_agw/search/s0123456789ab", "", "", nil); code != 200 {
		t.Fatalf("status over REST: %d %s", code, out)
	}
	if len(b.reqs) != 2 || !strings.Contains(b.reqs[0], "POST /_agw/search|") || !strings.Contains(b.reqs[1], "GET /_agw/search/s0123456789ab|") {
		t.Fatalf("what reached the backend: %v", b.reqs)
	}
	// A body in the old shape of the raw call, with notebook fields, is refused before the backend.
	code, out, _ := apiCall(t, c, "POST", "/platform-api/_agw/search", "application/json", `{"title":"t","path":"notebook","entry":"train.py"}`, nil)
	if code != 400 || !strings.Contains(out, "entry is not available in the REST binding") {
		t.Errorf("notebook search over REST: %d %s", code, out)
	}
}

// The arguments of the gateway paths are answered inside the gateway: no call to the platform.
func TestRESTGatewayDetail(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-rd", decide: "approved"}}
	c := start(t, b)
	code, out, _ := apiCall(t, c, "GET", "/platform-api/_agw/paths?prefix=/_agw/search", "", "", nil)
	if code != 200 || !strings.Contains(out, "search_id* (path, string)") {
		t.Fatalf("detail: %d %s", code, out)
	}
	if len(b.reqs) != 0 {
		t.Errorf("the detail reached the backend: %v", b.reqs)
	}
}

func TestRESTGatewayUnknown(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-ru", decide: "approved"}}
	c := start(t, b)
	for _, p := range []string{"/platform-api/_agw/nothing", "/platform-api/_AGW/search", "/platform-api/_agw/search/" + "s0123456789ab/unknown"} {
		code, out, _ := apiCall(t, c, "GET", p, "", "", nil)
		if code != 400 || !strings.Contains(out, "not a gateway path of the REST binding") {
			t.Errorf("%s: %d %s", p, code, out)
		}
	}
	if len(b.reqs) != 0 {
		t.Errorf("a refused call reached the backend: %v", b.reqs)
	}
}

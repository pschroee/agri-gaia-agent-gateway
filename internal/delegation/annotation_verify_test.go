// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"testing"

	"agw/internal/platform"
)

// The rollback is a write on the dataset the source belongs to (V3b, E21); the body carries only the
// source and the extent, and which objects that means is decided by the gateway.
func TestClassifyRollback(t *testing.T) {
	req := platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/rollbacks",
		Body: json.RawMessage(`{"source_id":"run-1","scope":"all"}`)}
	got := Classify(req)
	if got.Action != Update || got.Resource != Dataset || got.Effect != EffectWrite || got.Problem != "" {
		t.Fatalf("rollback: %+v", got)
	}
	// Without a scope the call is the same: the default is the extent of the tool, not of the route.
	got = Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/rollbacks",
		Body: json.RawMessage(`{"source_id":"run-1"}`)})
	if got.Effect != EffectWrite || got.Problem != "" {
		t.Errorf("without a scope: %+v", got)
	}
}

// A rollback without a source, or with an extent the gateway does not know, is refused before anything
// runs (V3b).
func TestRollbackBodyRefused(t *testing.T) {
	call := func(body string) Access {
		return Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/rollbacks",
			Body: json.RawMessage(body)})
	}
	if a := call(`{}`); a.Problem == "" {
		t.Errorf("a rollback without a source was let through: %+v", a)
	}
	if a := call(`{"source_id":"run-1","scope":"everything"}`); a.Problem == "" {
		t.Errorf("an unknown extent was let through: %+v", a)
	}
	if a := call(`{"source_id":"job-2","scope":"suspicious"}`); a.Problem != "" {
		t.Errorf("a known extent was refused: %s", a.Problem)
	}
}

// The bodies of the verification and of the rollback carry only the fields the tools build (V3b): the
// function, the mapping and the threshold of a verification of the judged source can never come from the
// agent, and the objects a rollback touches cannot come from the call.
func TestAnnotationVerifyFieldRules(t *testing.T) {
	checks := func(body string) Access {
		return Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/checks",
			Body: json.RawMessage(body)})
	}
	if a := checks(`{"mode":"verify","source_id":"run-1","final":true}`); a.Problem != "" {
		t.Errorf("the tool's own body was refused: %s", a.Problem)
	}
	if a := checks(`{"mode":"verify","source_id":"run-1","cvat_ids":[1,2]}`); a.Problem == "" {
		t.Errorf("a body naming the objects was let through: %+v", a)
	}
	rollbacks := func(body string) Access {
		return Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/rollbacks",
			Body: json.RawMessage(body)})
	}
	if a := rollbacks(`{"source_id":"run-1","scope":"all","cvat_ids":[1,2]}`); a.Problem == "" {
		t.Errorf("a rollback naming the objects itself was let through: %+v", a)
	}
}

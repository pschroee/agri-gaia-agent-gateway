// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"errors"
	"testing"
)

func TestOutcomeOf(t *testing.T) {
	for _, c := range []struct {
		name string
		res  Result
		err  error
		want CallOutcome
	}{
		{"ok 200", Result{Status: "ok", HTTPStatus: 200}, nil, OutcomeOK},
		{"ok without HTTP (gateway answer)", Result{Status: "ok"}, nil, OutcomeOK},
		{"ok 404", Result{Status: "ok", HTTPStatus: 404}, nil, OutcomeFailed},
		{"error", Result{Status: "error", Message: "x"}, nil, OutcomeFailed},
		{"denied", Result{Status: "denied"}, nil, OutcomeFailed},
		{"unavailable", Result{Status: StatusUnavailable}, nil, OutcomeFailed},
		{"rejected", Result{Status: "rejected"}, nil, OutcomeNeutral},
		{"pending", Result{Status: StatusPendingUser}, nil, OutcomeNeutral},
		{"Go error", Result{Status: "ok", HTTPStatus: 200}, errors.New("boom"), OutcomeFailed},
	} {
		if got := OutcomeOf(c.res, c.err); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCallKey(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/datasets/5", "get_dataset"},
		{"get", "/datasets/5/", "get_dataset"},
		{"POST", "/_agw/plan", "propose_plan"},
		{"POST", "/_AGW/plan", "propose_plan"},
		{"PUT", "/container-deployments/3/deploy", "start_deployment"},
		{"GET", "/models/7", "get_model"}, // get_model and validate_model share the form; the first name wins
		{"GET", "/foo/12/bar", "GET /foo/{id}/bar"},
		{"GET", "/foo/11111111-2222-3333-4444-555555555555", "GET /foo/{id}"},
		{"GET", "/datasets", "list_datasets"},
	} {
		if got := CallKey(c.method, c.path); got != c.want {
			t.Errorf("CallKey(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestAppendAgentNote(t *testing.T) {
	// Without an HTTP status Result.Text prints only the message.
	r := AppendAgentNote(Result{Status: "error", Message: "refused: x"}, "NOTE")
	if r.Message != "refused: x\nNOTE" || r.Note != "" {
		t.Fatalf("HTTP 0: %+v", r)
	}
	// With one, the note follows the answer.
	r = AppendAgentNote(Result{Status: "ok", HTTPStatus: 404, Body: "b"}, "NOTE")
	if r.Note != "NOTE" || r.Message != "" {
		t.Fatalf("HTTP 404: %+v", r)
	}
	r = AppendAgentNote(Result{Status: "ok", HTTPStatus: 404, Note: "old"}, "NOTE")
	if r.Note != "old\nNOTE" {
		t.Fatalf("existing note: %+v", r)
	}
	// Denied and rejected answers print the message only, whatever the HTTP status.
	if r = AppendAgentNote(Result{Status: "denied", Message: "m", HTTPStatus: 403}, "NOTE"); r.Message != "m\nNOTE" {
		t.Fatalf("denied: %+v", r)
	}
	if r = AppendAgentNote(Result{Status: "error", Message: "m"}, ""); r.Message != "m" {
		t.Fatalf("empty note: %+v", r)
	}
}

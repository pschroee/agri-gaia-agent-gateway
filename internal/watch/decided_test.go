// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"agw/internal/platform"
)

// answering is a reader that always answers with the same result.
func answering(res platform.Result) Reader {
	return func(context.Context, platform.Request) (platform.Result, error) { return res, nil }
}

// failing is a reader whose call does not go out (network, timeout, a lost login).
func failing(err error) Reader {
	return func(context.Context, platform.Request) (platform.Result, error) { return platform.Result{}, err }
}

// Issue #140: a kind says whether a failed read is decided — the platform answered and the object is gone
// (404, an answer that is no object any more) — or transient (5xx, the call never went out).
func TestDecidedReadErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		read    Reader
		decided bool
	}{
		{"task gone (404)", answering(platform.Result{Status: "error", HTTPStatus: http.StatusNotFound, Message: "platform responds 404"}), true},
		{"task answer is no task", answering(platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: `{"status":`}), true},
		{"task empty answer", answering(platform.Result{Status: "ok", HTTPStatus: http.StatusOK}), true},
		{"task server error (500)", answering(platform.Result{Status: "error", HTTPStatus: http.StatusInternalServerError, Message: "platform responds 500"}), false},
		{"task login gone (401)", answering(platform.Result{Status: "error", HTTPStatus: http.StatusUnauthorized, Message: "platform responds 401"}), false},
		{"task never asked", failing(errors.New("dial tcp: connection refused")), false},
	}
	for _, c := range cases {
		_, err := pollPlatformTask(ctx, Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5"}, c.read)
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if got := IsDecided(err); got != c.decided {
			t.Errorf("%s: IsDecided = %v, want %v (%v)", c.name, got, c.decided, err)
		}
	}
	// The training container follows the same rule.
	_, err := pollTrainingRun(ctx, Job{ID: "w2", Kind: kindTrainingRun, Ref: "9"},
		answering(platform.Result{Status: "error", HTTPStatus: http.StatusNotFound, Message: "platform responds 404"}))
	if err == nil || !IsDecided(err) {
		t.Fatalf("a removed training container must be decided: %v", err)
	}
	_, err = pollTrainingRun(ctx, Job{ID: "w2", Ref: "9"},
		answering(platform.Result{Status: "error", HTTPStatus: http.StatusServiceUnavailable, Message: "platform responds 503"}))
	if err == nil || IsDecided(err) {
		t.Fatalf("a 503 must stay transient: %v", err)
	}
	_, err = pollTrainingRun(ctx, Job{ID: "w2", Ref: "9"},
		answering(platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: `not json`}))
	if err == nil || !IsDecided(err) {
		t.Fatalf("an answer that is no container must be decided: %v", err)
	}
	// Wrapping keeps the message the kind wrote (the job shows it).
	base := errors.New("task 5 not readable: platform responds 404")
	if got := Decided(base).Error(); got != base.Error() {
		t.Fatalf("the message changed: %q", got)
	}
	if !errors.Is(Decided(base), base) {
		t.Fatal("the wrapped error is not reachable")
	}
	if Decided(nil) != nil || IsDecided(nil) {
		t.Fatal("nil is no decided error")
	}
}

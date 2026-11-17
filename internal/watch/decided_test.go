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

// The two kinds of 404 (issue #259): the backend answers in the FastAPI error form, the proxy in front of it
// answers a plain text while the backend restarts or is unhealthy.
var (
	backend404 = platform.Result{Status: "error", HTTPStatus: http.StatusNotFound, Message: "platform responds 404", Body: `{"detail":"Not Found"}`}
	proxy404   = platform.Result{Status: "error", HTTPStatus: http.StatusNotFound, Message: "platform responds 404", Body: "404 page not found"}
)

// Issue #140: a kind says whether a failed read is decided — the platform answered and the object is gone
// (404, an answer that is no object any more) — or transient (5xx, the call never went out).
func TestDecidedReadErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		read    Reader
		decided bool
	}{
		{"task gone (404)", answering(backend404), true},
		{"task behind a proxy 404", answering(proxy404), false},
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
		answering(backend404))
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

// Issue #259: only a 404 that the backend wrote (JSON with "detail") ends a job.
func TestDecidedByAnswer(t *testing.T) {
	cases := []struct {
		name string
		res  platform.Result
		want bool
	}{
		{"404 with FastAPI detail", platform.Result{HTTPStatus: 404, Body: `{"detail":"Not Found"}`}, true},
		{"404 with a structured detail", platform.Result{HTTPStatus: 404, Body: `{"detail":{"msg":"x"}}`}, true},
		{"404 from the proxy (plain text)", platform.Result{HTTPStatus: 404, Body: "404 page not found"}, false},
		{"404 empty", platform.Result{HTTPStatus: 404}, false},
		{"404 JSON without detail", platform.Result{HTTPStatus: 404, Body: `{"error":"x"}`}, false},
		{"404 JSON array", platform.Result{HTTPStatus: 404, Body: `[]`}, false},
		{"500 with detail", platform.Result{HTTPStatus: 500, Body: `{"detail":"boom"}`}, false},
		{"401 with detail", platform.Result{HTTPStatus: 401, Body: `{"detail":"Not authenticated"}`}, false},
		{"200", platform.Result{HTTPStatus: 200, Body: `{"detail":"x"}`}, false},
	}
	for _, c := range cases {
		if got := DecidedByAnswer(c.res); got != c.want {
			t.Errorf("%s: DecidedByAnswer = %v, want %v", c.name, got, c.want)
		}
	}
}

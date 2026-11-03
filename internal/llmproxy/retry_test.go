// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/config"
)

func retryProxy(t *testing.T, rec *fakeRecorder, failures int32, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var bodies atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) == `{"model":"m1"}` {
			bodies.Add(1)
		}
		if hits.Add(1) <= failures {
			w.WriteHeader(status)
			io.WriteString(w, `{"message":"Service is too busy"}`)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"id":"r","model":"m1","choices":[]}`)
	}))
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk")
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m1","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY","models":[{"id":"m1"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	px := New(cat)
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	t.Cleanup(s.Close)
	return s, &hits
}

func fastRetry(t *testing.T, budget time.Duration) {
	t.Helper()
	ob, ow := retryBudget, retryWait
	retryBudget, retryWait = budget, func(int) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() { retryBudget, retryWait = ob, ow })
}

// BM9: an experiment run gets through a short outage of the provider; the body is sent again in full.
func TestExperimentRetriesTransientUpstreamError(t *testing.T) {
	fastRetry(t, time.Minute)
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true}}
	srv, hits := retryProxy(t, rec, 3, 503)
	resp, body := postModel(t, srv, `{"model":"m1"}`)
	if resp.StatusCode != 200 || hits.Load() != 4 {
		t.Fatalf("status %d after %d hits: %s", resp.StatusCode, hits.Load(), body)
	}
	if len(rec.calls) != 1 || rec.calls[0].Status != 200 {
		t.Fatalf("only the answered call is recorded: %+v", rec.calls)
	}
}

// A product chat gets the provider's answer unchanged.
func TestProductChatIsNotRetried(t *testing.T) {
	fastRetry(t, time.Minute)
	rec := &fakeRecorder{chat: "chat-1"}
	srv, hits := retryProxy(t, rec, 3, 503)
	resp, _ := postModel(t, srv, `{"model":"m1"}`)
	if resp.StatusCode != 503 || hits.Load() != 1 {
		t.Fatalf("status %d after %d hits", resp.StatusCode, hits.Load())
	}
}

// When the outage outlasts the budget the last answer goes through.
func TestRetryBudgetEnds(t *testing.T) {
	fastRetry(t, 40*time.Millisecond)
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true}}
	srv, hits := retryProxy(t, rec, 1000, 503)
	resp, _ := postModel(t, srv, `{"model":"m1"}`)
	if resp.StatusCode != 503 || hits.Load() < 2 || hits.Load() > 20 {
		t.Fatalf("status %d after %d hits", resp.StatusCode, hits.Load())
	}
}

// A refusal that is no outage (400) is not repeated.
func TestRetryNotForClientErrors(t *testing.T) {
	fastRetry(t, time.Minute)
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true}}
	srv, hits := retryProxy(t, rec, 5, 400)
	resp, _ := postModel(t, srv, `{"model":"m1"}`)
	if resp.StatusCode != 400 || hits.Load() != 1 {
		t.Fatalf("status %d after %d hits", resp.StatusCode, hits.Load())
	}
}

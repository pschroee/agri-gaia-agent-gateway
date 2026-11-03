// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Upstream outages in an experiment run (BM9): the pilot of 3 Nov 2026 got "503 Service is too busy" from the
// model provider on four calls within 16 seconds (pi's own retries); the turn ended and the run with it, 90 minutes
// into a training the agent was waiting for. A run must not depend on how busy a provider is for a quarter of a
// minute, so the proxy repeats a call that the provider answered with a transient error, with growing waits, for
// up to retryBudget. This holds for every model alike. The failed attempts have no tokens and are not recorded;
// the wait counts into the run's duration.
var (
	retryBudget = 15 * time.Minute
	retryWait   = func(attempt int) time.Duration { // 5 s, 10 s, 20 s, then 30 s
		d := 5 * time.Second << attempt
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d
	}
)

// retryStatus: answers that say "try again later" (overloaded, bad gateway, unavailable, timeout, rate limit).
func retryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// retryTransport repeats a request whose answer is a transient error status or whose connection failed, as long
// as the budget lasts. The request body is replayed from body.
type retryTransport struct {
	base http.RoundTripper
	body func() io.ReadCloser
	log  string // provider, for the log
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	for attempt := 0; ; attempt++ {
		r := req
		if attempt > 0 {
			r = req.Clone(req.Context())
			r.Body = t.body()
		}
		resp, err := t.base.RoundTrip(r)
		transient := err != nil || retryStatus(resp.StatusCode)
		wait := retryWait(attempt)
		if !transient || time.Since(start)+wait > retryBudget || req.Context().Err() != nil {
			return resp, err
		}
		status := 0
		if err == nil {
			status = resp.StatusCode
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		}
		slog.Warn("LLM proxy: transient upstream error, retrying", "provider", t.log, "status", status, "error", err,
			"attempt", attempt+1, "wait", wait)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}
}

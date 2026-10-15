// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DoTrusted performs a call that the gateway itself needs for one operation of a feature (E5): the
// agent approves the operation as a whole and never names one of these calls. Two differences to Do:
//
//   - the registered blockers do not run (normalize with agent false), so that a feature reaches the
//     paths it keeps the agent away from (e.g. /cvat/auth/login for the CVAT session);
//   - the response may be larger (limit instead of maxRead) and comes back as raw bytes as well, so
//     that the feature can read it itself (a ZIP of the platform, the JSON of the CVAT login). What
//     the agent could see (Result.Body) is redacted and truncated exactly as in Do.
//
// Every call is logged by the caller as "annotation step" and listed in the answer of its operation
// (E5). The Result never carries the answer of the platform unchanged.
func (c *Client) DoTrusted(ctx context.Context, chatID string, r Request, limit int64) (Result, []byte, error) {
	if c == nil {
		return Result{}, nil, ErrNotConfigured
	}
	r, err := normalize(r, false)
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, nil, nil
	}
	if limit <= 0 {
		limit = maxRead
	}
	ctx = withTrustedHTTP(ctx, c.trustedHTTP())
	resp, err := c.send(ctx, chatID, r, false)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		resp, err = c.send(ctx, chatID, r, true)
	}
	if err != nil {
		return Result{}, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Result{}, nil, err
	}
	if int64(len(raw)) > limit {
		// Truncated JSON could no longer be redacted; then none of it is passed on (Review 5, W4).
		res := Result{Status: "ok", HTTPStatus: resp.StatusCode, Truncated: true,
			Body: fmt.Sprintf("[response larger than %d MiB; not passed on]", limit>>20)}
		if resp.StatusCode >= 400 {
			res.Status, res.Message = "error", fmt.Sprintf("platform responds %d", resp.StatusCode)
		}
		return res, nil, nil
	}
	res := Result{Status: "ok", HTTPStatus: resp.StatusCode, Location: c.relLocation(resp.Header.Get("Location"))}
	body := raw
	if ct := resp.Header.Get("Content-Type"); len(body) > 0 && !textual(ct) {
		res.Body = fmt.Sprintf("[binary response, %d bytes, %s; not passed on]", len(body), ct)
	} else {
		if json.Valid(body) {
			body = redact(body)
		}
		if len(body) > c.cfg.MaxResult && !r.Full {
			body, res.Truncated = body[:c.cfg.MaxResult], true
		}
		res.Body = strings.ToValidUTF8(string(body), "")
	}
	if resp.StatusCode >= 400 {
		res.Status, res.Message = "error", fmt.Sprintf("platform responds %d", resp.StatusCode)
	}
	return res, raw, nil
}

// TrustedHTTPTimeout bounds one call of a feature. It is much longer than the 60 s of the agent's
// calls: POST /cvat/tasks/{id} makes the backend create a CVAT task and wait until CVAT has imported
// the images of the dataset.
const TrustedHTTPTimeout = 10 * time.Minute

// trustedHTTP is the client for such a call: the same transport as the configured client (connections
// and TLS settings stay shared), without its 60 s timeout and without following redirects.
func (c *Client) trustedHTTP() *http.Client {
	return &http.Client{
		Transport:     c.cfg.HTTP.Transport,
		Timeout:       TrustedHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// trustedHTTPKey is the context key of the client of a feature's call.
type trustedHTTPKey struct{}

// withTrustedHTTP puts a client of its own into the context; send uses it instead of the configured
// one, so that the feature's call is not cut off after 60 s.
func withTrustedHTTP(ctx context.Context, hc *http.Client) context.Context {
	return context.WithValue(ctx, trustedHTTPKey{}, hc)
}

// trustedHTTPFrom reads the client of a feature's call; nil for the agent's calls.
func trustedHTTPFrom(ctx context.Context) *http.Client {
	hc, _ := ctx.Value(trustedHTTPKey{}).(*http.Client)
	return hc
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"agw/internal/platform"
)

// Read errors of a kind (issue #140). A poll that fails says whether the failure is **decided**: the
// platform answered and the job can never become readable again — the task is gone (a 404 from the backend itself), or its answer is
// no longer a task (unparsable JSON). The loop ends such a job as "failed" with the reason. Every other
// error (network, timeout, 5xx, a lost login) is **transient**: the job stays running, the failures are
// counted, the next attempt waits longer and the log gets one warning, not one per tick.
//
// A kind that wraps nothing is read as transient — failing closed in the safe direction: an unknown error
// never ends a job.

// decidedError marks a read error that ends the job.
type decidedError struct{ err error }

func (e *decidedError) Error() string { return e.err.Error() }
func (e *decidedError) Unwrap() error { return e.err }

// Decided marks a read error as one that ends the job (the platform answered and the job is gone).
func Decided(err error) error {
	if err == nil {
		return nil
	}
	return &decidedError{err: err}
}

// Decidedf is Decided with a formatted message.
func Decidedf(format string, args ...any) error {
	return &decidedError{err: fmt.Errorf(format, args...)}
}

// IsDecided reports whether a read error ends the job.
func IsDecided(err error) bool {
	var d *decidedError
	return errors.As(err, &d)
}

// DecidedByAnswer reports whether an answer says that the job is gone: a 404 that the **backend** wrote
// (issue #259). The backend answers in the FastAPI error form, a JSON object with a "detail" key. The proxy
// in front of it answers a plain-text "404 page not found" while the backend restarts or is unhealthy; that
// says nothing about the job and stays transient. Every other status is transient too — a 5xx or a timeout
// may pass, and a 401 or 403 is the user's login, which comes back.
func DecidedByAnswer(res platform.Result) bool {
	if res.HTTPStatus != http.StatusNotFound {
		return false
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		return false
	}
	_, ok := body["detail"]
	return ok
}

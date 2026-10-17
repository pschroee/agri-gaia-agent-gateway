// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"errors"
	"fmt"
	"net/http"
)

// Read errors of a kind (issue #140). A poll that fails says whether the failure is **decided**: the
// platform answered and the job can never become readable again — the task is gone (404), or its answer is
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

// DecidedByStatus reports whether an answer says that the job is gone: a 404 on the object the kind reads.
// Every other status is transient — a 5xx or a timeout may pass, and a 401 or 403 is the user's login,
// which comes back.
func DecidedByStatus(status int) bool { return status == http.StatusNotFound }

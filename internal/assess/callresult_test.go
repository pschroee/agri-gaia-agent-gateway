// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"errors"
	"strings"
	"testing"

	"agw/internal/platform"
)

// #263: a successful internal call is logged like the socket logs it ("ok 200"), so the activity does not
// list the reads of the checks as errors.
func TestCallResultOkFormat(t *testing.T) {
	if got := callResult(platform.Result{Status: "ok", HTTPStatus: 200}, nil); got != "ok 200" {
		t.Errorf("success: %q", got)
	}
	if got := callResult(platform.Result{Status: "ok", HTTPStatus: 201}, nil); got != "ok 201" {
		t.Errorf("created: %q", got)
	}
	if got := callResult(platform.Result{}, errors.New("boom")); got != "error: boom" {
		t.Errorf("error: %q", got)
	}
	if got := callResult(platform.Result{Status: "error", Message: "platform responds 404"}, nil); !strings.HasPrefix(got, "error: ") {
		t.Errorf("failed call: %q", got)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// LoggedCall is one row of the gateway's call log (socket_calls) as far as no_attempt needs it. Every call
// of the agent to the platform is logged there, whatever the binding (mcp, cli, api) and whatever became of
// it: allowed, blocked by the delegation, rejected, refused by the gateway or answered by the platform with an
// error. For op "platform" the detail reads "METHOD /path[?query][ (multipart…)]".
type LoggedCall struct {
	Via    string `json:"via"`
	Op     string `json:"op"`
	Detail string `json:"detail"`
	Result string `json:"result"`
}

// Attempt is a platform call the agent made, taken from a LoggedCall.
type Attempt struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Via    string `json:"via"`
	Result string `json:"result"`
}

var attemptDetailRe = regexp.MustCompile(`^([A-Z]+) (/[^ ?]*)`)

// ParseAttempt reads the method and the path of a platform call from the log. Other entries (internet,
// uploads, a refusal that names only the tool) are not attempts at a path and give ok false.
func ParseAttempt(c LoggedCall) (Attempt, bool) {
	if c.Op != "platform" {
		return Attempt{}, false
	}
	m := attemptDetailRe.FindStringSubmatch(c.Detail)
	if m == nil {
		return Attempt{}, false
	}
	return Attempt{Method: m[1], Path: normalizeAttemptPath(m[2]), Via: c.Via, Result: c.Result}, true
}

// normalizeAttemptPath drops the API prefix the material may write and a trailing slash, so that
// /api/datasets/3/toggle-public/ and /datasets/3/toggle-public compare equal.
func normalizeAttemptPath(p string) string {
	if p == "/api" {
		return "/"
	}
	p = strings.TrimPrefix(p, "/api/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

func (a AttemptPattern) validate() error {
	m := strings.ToUpper(strings.TrimSpace(a.Method))
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "*", "NOTEBOOK":
	case "":
		return errors.New("method is empty (GET, POST, PUT, PATCH, DELETE, NOTEBOOK or *)")
	default:
		return fmt.Errorf("method must be GET, POST, PUT, PATCH, DELETE, NOTEBOOK or *, not %q", a.Method)
	}
	if !strings.HasPrefix(a.Path, "/") {
		return fmt.Errorf("path must start with /, not %q", a.Path)
	}
	if strings.Contains(a.Path, "//") || strings.ContainsAny(a.Path, "? ") {
		return fmt.Errorf("path %q must have no empty segment, query or space", a.Path)
	}
	if _, err := path.Match(strings.ReplaceAll(a.Path, "/", "|"), "|"); err != nil {
		return fmt.Errorf("path %q is not a valid pattern: %v", a.Path, err)
	}
	return nil
}

// Matches reports whether the attempt is the call the pattern names. The method compares without
// regard to case, * as method matches any. In the path, * stands for exactly one segment (it may also
// stand for part of a segment, as in path.Match); the number of segments must be equal.
func (a AttemptPattern) Matches(t Attempt) bool {
	m := strings.ToUpper(strings.TrimSpace(a.Method))
	if m != "*" && m != t.Method {
		return false
	}
	want := strings.Split(strings.Trim(normalizeAttemptPath(a.Path), "/"), "/")
	got := strings.Split(strings.Trim(t.Path, "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		ok, err := path.Match(want[i], got[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}

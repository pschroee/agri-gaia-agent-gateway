// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"encoding/json"
	"testing"
)

func TestEscapeNULs(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":"x"}`, `{"a":"x"}`},
		{`{"a":"x\u0000y"}`, `{"a":"x\ufffdy"}`},
		{`{"a":"\u0000\u0000"}`, `{"a":"\ufffd\ufffd"}`},
		{`{"a":"lit \\u0000 text"}`, `{"a":"lit \\u0000 text"}`},
		{`{"a":"\\\u0000"}`, `{"a":"\\\ufffd"}`},
	}
	for _, c := range cases {
		got := string(escapeNULs(json.RawMessage(c.in)))
		if got != c.want {
			t.Errorf("escapeNULs(%s) = %s, want %s", c.in, got, c.want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("not valid JSON: %s", got)
		}
	}
}

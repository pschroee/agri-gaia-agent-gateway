// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

// Control and bidi characters appear visibly in a text the user is shown (Review W1); newline and tab stay.
func TestVisibleControls(t *testing.T) {
	rlo := string(rune(0x202e)) // RIGHT-TO-LEFT OVERRIDE
	if s := VisibleControls("a" + rlo + "b\nc\td"); s != "a\\u202eb\nc\td" {
		t.Errorf("plain text: %q", s)
	}
	if s := VisibleControls("a\x01b"); s != "a\\u0001b" {
		t.Errorf("control character: %q", s)
	}
}

// In a text that is itself JSON the escape carries an extra backslash: a reader that parses the JSON gets
// the visible \u sequence and not the character back. Everything else stays byte for byte.
func TestVisibleControlsJSON(t *testing.T) {
	rlo := string(rune(0x202e))
	in := `{"cells":[{"source":"a` + rlo + `b"}]}`
	out := VisibleControlsJSON(in)
	if !strings.Contains(out, `a\\u202eb`) {
		t.Fatalf("not escaped: %q", out)
	}
	var nb struct {
		Cells []struct {
			Source string `json:"source"`
		} `json:"cells"`
	}
	if err := json.Unmarshal([]byte(out), &nb); err != nil || len(nb.Cells) != 1 {
		t.Fatalf("the escaped text is no JSON any more: %q %v", out, err)
	}
	// Parsing gives the visible sequence, never the character again.
	if nb.Cells[0].Source != `a‮b` {
		t.Errorf("after parsing the JSON: %q", nb.Cells[0].Source)
	}
	// A notebook without such characters is byte for byte the file the user approved.
	plain := `{"cells":[{"source":"print(1)\n"}]}`
	if s := VisibleControlsJSON(plain); s != plain {
		t.Errorf("plain JSON changed: %q", s)
	}
}

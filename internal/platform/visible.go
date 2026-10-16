// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"strings"
)

// Text that the gateway shows the user about a call is set as plain text in the UI, and a control or
// bidi character in it would reorder or hide what the user reads (Review W1). Both functions write such
// characters as \u sequences; newline and tab stay. They live here because more than the preview of a
// call carries such text: the structured approval data of a notebook run (own code C-E4) carries the
// code of the run file by file.

// VisibleControls escapes control and bidi characters for a plain text.
func VisibleControls(s string) string { return escapeControls(s, false) }

// VisibleControlsJSON escapes control and bidi characters for a text that is itself JSON (the cells of a
// notebook): the escape carries one extra backslash, so a reader that parses the JSON gets the visible
// \u sequence back and not the character. Nothing else changes, so a notebook without such characters
// stays byte for byte the file the user approved.
func VisibleControlsJSON(s string) string { return escapeControls(s, true) }

// escapeControls does the work; inJSON adds the extra backslash in front of the escape.
func escapeControls(s string, inJSON bool) string {
	escape := "\\u"
	if inJSON {
		escape = "\\\\u"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			fmt.Fprintf(&b, "%s%04x", escape, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package imagereview

import "strings"

// CutObject cuts the JSON object out of the model's answer: a fence or a sentence around it is dropped, and
// what stands between the outermost braces is the caller's fixed JSON — no JSON, no verdict.
func CutObject(text string) ([]byte, bool) {
	s := strings.TrimSpace(text)
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return nil, false
	}
	return []byte(s[i : j+1]), true
}

// OneLine cuts a text to at most max characters and writes it as one line.
func OneLine(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

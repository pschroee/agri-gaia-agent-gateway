// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"fmt"
	"strings"
)

// MaxDiffLines bounds the diff of one changed file in an approval; a longer one is not shown.
const MaxDiffLines = 400

const diffContext = 3

// UnifiedDiff compares two texts line by line and returns the changed lines with three lines of context, in the
// style of a unified diff ("@@ -old,+new @@", " ", "-", "+"). tooLong is true when the diff has more than
// MaxDiffLines lines (or the files are too large to compare); the diff is then empty and the caller shows the whole
// new file instead.
func UnifiedDiff(oldText, newText string) (diff string, tooLong bool) {
	a, b := splitLines(oldText), splitLines(newText)
	if len(a) > 3000 || len(b) > 3000 {
		return "", true
	}
	// LCS table.
	n, m := len(a), len(b)
	l := make([][]int32, n+1)
	for i := range l {
		l[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				l[i][j] = l[i+1][j+1] + 1
			} else if l[i+1][j] >= l[i][j+1] {
				l[i][j] = l[i+1][j]
			} else {
				l[i][j] = l[i][j+1]
			}
		}
	}
	type line struct {
		op   byte
		text string
		a, b int // 1-based numbers in the old and new file
	}
	var all []line
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			all = append(all, line{' ', a[i], i + 1, j + 1})
			i++
			j++
		case j < m && (i == n || l[i][j+1] >= l[i+1][j]):
			all = append(all, line{'+', b[j], i + 1, j + 1})
			j++
		default:
			all = append(all, line{'-', a[i], i + 1, j + 1})
			i++
		}
	}
	keep := make([]bool, len(all))
	for k, ln := range all {
		if ln.op == ' ' {
			continue
		}
		for d := -diffContext; d <= diffContext; d++ {
			if k+d >= 0 && k+d < len(all) {
				keep[k+d] = true
			}
		}
	}
	var out []string
	inHunk := false
	for k, ln := range all {
		if !keep[k] {
			inHunk = false
			continue
		}
		if !inHunk {
			out = append(out, fmt.Sprintf("@@ -%d +%d @@", ln.a, ln.b))
			inHunk = true
		}
		out = append(out, string(ln.op)+ln.text)
	}
	if len(out) > MaxDiffLines {
		return "", true
	}
	return strings.Join(out, "\n"), false
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

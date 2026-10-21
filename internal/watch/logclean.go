// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package watch

import (
	"regexp"
	"strings"
)

// progressLineRe matches a tqdm progress line with or without a total ("| 17/17 [00:03", "| 0/? [00:00",
// "0it [00:00").
var progressLineRe = regexp.MustCompile(`(?:\|\s*\d+/(?:\d+|\?)|\b\d+it)\s*\[\d{2}:\d{2}`)

// ansiRe matches ANSI CSI escape sequences.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// CollapseProgress cleans a log for display and for the error check (issue #178). tqdm redraws its bar with
// a carriage return inside one "\n" line, so one line can hold dozens of states and fill the whole tail.
// It strips ANSI sequences, turns "\r\n" into "\n", keeps the last non-empty "\r" segment of every line and
// reduces runs of consecutive progress lines to the last one. It is idempotent.
func CollapseProgress(s string) string {
	if s == "" {
		return ""
	}
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	prevProgress := false
	for _, line := range lines {
		if strings.Contains(line, "\r") {
			segs := strings.Split(line, "\r")
			last := ""
			for i := len(segs) - 1; i >= 0; i-- {
				if strings.TrimSpace(segs[i]) != "" {
					last = segs[i]
					break
				}
			}
			line = last
		}
		line = strings.TrimRight(line, " \t")
		isProgress := progressLineRe.MatchString(line)
		if isProgress && prevProgress {
			out[len(out)-1] = line
		} else {
			out = append(out, line)
		}
		prevProgress = isProgress
	}
	return strings.Join(out, "\n")
}

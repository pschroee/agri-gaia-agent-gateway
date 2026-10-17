// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package exectrace parses the lines of a syscall trace that the supervisor agw-exec writes
// with strace (X6). The supervisor traces only failed syscalls (--status=failed) and prefixes
// every line with the pid (-f) and an epoch timestamp (-ttt):
//
//	325 1759234567.123456 openat(AT_FDCWD, "/workspace/x", O_RDONLY) = -1 ENOENT (No such file or directory)
//
// What does not match this form (e.g. a partial last line) becomes one row with Syscall "?" and
// the raw line as Args: the evaluation then sees that something was there that the parser does
// not know, instead of the line silently vanishing (X6.4).
package exectrace

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Syscall is one failed syscall. TS is the zero time when the line carried no timestamp.
type Syscall struct {
	PID   int
	Name  string
	Args  string
	Errno string
	TS    time.Time
}

// Unknown is the name of unparsable lines (X6.4).
const Unknown = "?"

// lineRe matches the form of strace with -f and -ttt. The argument list may itself contain
// parentheses, so the group is greedy and the match anchors on "= -1".
var lineRe = regexp.MustCompile(`^\s*(\d+)\s+(\d+(?:\.\d+)?)\s+([A-Za-z_][A-Za-z0-9_]*)\((.*)\)\s+=\s+-1\s+([A-Z][A-Z0-9_]*)(?:\s+\(.*\))?\s*$`)

// Parse splits a trace into syscall rows.
func Parse(raw []byte) []Syscall {
	if len(raw) == 0 {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	out := make([]Syscall, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := lineRe.FindStringSubmatch(line); m != nil {
			pid, _ := strconv.Atoi(m[1])
			s := Syscall{PID: pid, Name: m[3], Args: m[4], Errno: m[5]}
			if sec, err := strconv.ParseFloat(m[2], 64); err == nil {
				whole, frac := math.Modf(sec)
				s.TS = time.Unix(int64(whole), int64(frac*1e9))
			}
			out = append(out, s)
			continue
		}
		out = append(out, Syscall{Name: Unknown, Args: strings.TrimSpace(line)})
	}
	return out
}

// modf splits seconds into whole seconds and the fractional part (time.Unix takes nanoseconds).
func modf(sec float64) (int64, float64) {
	whole := int64(sec)
	return whole, sec - float64(whole)
}

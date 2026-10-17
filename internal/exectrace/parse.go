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
			if ts, ok := parseTS(m[2]); ok {
				s.TS = ts
			}
			out = append(out, s)
			continue
		}
		out = append(out, Syscall{Name: Unknown, Args: strings.TrimSpace(line)})
	}
	return out
}

// parseTS turns strace's epoch timestamp ("1759234567.123456", -ttt) into a time.Time. It parses
// the decimal digits exactly instead of going through float64: the float conversion of
// 1759234567.123456 lands one nanosecond next to the value strace printed.
func parseTS(s string) (time.Time, bool) {
	sec, frac, hasFrac := strings.Cut(s, ".")
	whole, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if !hasFrac {
		return time.Unix(whole, 0), true
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	ns, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(whole, ns), true
}

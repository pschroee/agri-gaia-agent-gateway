// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"strings"
	"testing"
	"time"
)

// A-E2 (D2.4): an estimate only from what the platform reported — at least two measurements, at least
// five percent, linear from the start.
func TestETA(t *testing.T) {
	start := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	one := []Sample{{At: start.Add(time.Minute), Percent: 50}}
	if got := ETA(start, one); got != nil {
		t.Fatalf("one measurement must not give an estimate: %v", got)
	}
	two := []Sample{one[0], {At: start.Add(2 * time.Minute), Percent: 50}}
	got := ETA(start, two)
	if got == nil {
		t.Fatal("two measurements must give an estimate")
	}
	// Half the work in two minutes: two more minutes to go.
	if want := start.Add(4 * time.Minute); !got.Equal(want) {
		t.Fatalf("eta = %v, want %v", got, want)
	}
	// Below five percent the estimate is too far off to name it.
	if got := ETA(start, []Sample{one[0], {At: start.Add(time.Minute), Percent: 3}}); got != nil {
		t.Fatalf("three percent must not give an estimate: %v", got)
	}
	// A hundred percent is the end itself; the job is done.
	if got := ETA(start, []Sample{one[0], {At: start.Add(time.Minute), Percent: 100}}); got != nil {
		t.Fatalf("a hundred percent must not give an estimate: %v", got)
	}
	// A quarter of the work in five minutes: fifteen more.
	got = ETA(start, []Sample{one[0], {At: start.Add(5 * time.Minute), Percent: 25}})
	if got == nil || !got.Equal(start.Add(20*time.Minute)) {
		t.Fatalf("eta = %v, want %v", got, start.Add(20*time.Minute))
	}
}

// The platform reports completion_percentage as a fraction 0..1 (tasks_api keeps 1.0 for a finished
// task); the job's percent is 0..100, nil while the platform reports nothing.
func TestPercent(t *testing.T) {
	if got := Percent(0); got != nil {
		t.Fatalf("zero is unknown, not zero percent: %v", got)
	}
	if got := Percent(0.4); got == nil || *got != 40 {
		t.Fatalf("fraction: %v", got)
	}
	if got := Percent(1); got == nil || *got != 100 {
		t.Fatalf("one is all of it: %v", got)
	}
	if got := Percent(120); got == nil || *got != 100 {
		t.Fatalf("more than all of it is capped: %v", got)
	}
}

// The log tail a job stores is bounded (D2.5): at most twenty lines and 4000 runes.
func TestLogTailBounded(t *testing.T) {
	var b []rune
	for i := 0; i < 30; i++ {
		b = append(b, ('a' + rune(i%26)), '\n')
	}
	out := Tail(string(b))
	if n := len(splitLines(out)); n != MaxLogLines {
		t.Fatalf("%d lines, want %d", n, MaxLogLines)
	}
	// The end is kept, not the start: the first of the twenty kept lines is the eleventh written one.
	if !strings.HasPrefix(out, string(rune('a'+10))+"\n") {
		t.Fatalf("the tail must keep the end: %q", out)
	}
	long := ""
	for i := 0; i < MaxLogRunes*2; i++ {
		long += "x"
	}
	out = Tail(long)
	if n := len([]rune(out)); n > MaxLogRunes+1 {
		t.Fatalf("%d runes, want at most %d", n, MaxLogRunes+1)
	}
	if len([]rune(out)) < MaxLogRunes-1 {
		t.Fatalf("the tail is too short: %d", len([]rune(out)))
	}
	if got := Tail(""); got != "" {
		t.Fatalf("empty log: %q", got)
	}
	// A platform message is one line of at most 200 runes.
	if got := oneLine("a\nb   c"); got != "a b c" {
		t.Fatalf("one line: %q", got)
	}
}

// splitLines counts the lines of a text the way the test needs it.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

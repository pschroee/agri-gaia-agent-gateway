// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lightning-test-tail.log is the real end of a Torchvision template log (17 Oct 2026); traceback-under-bars.log
// is constructed: a traceback with 30 redrawn epoch bars below it.
func TestCollapseProgressRealLightningTail(t *testing.T) {
	raw := readTestdata(t, "lightning-test-tail.log")
	if !strings.Contains(raw, "\r") {
		t.Fatal("the test file lost its carriage returns")
	}
	out := CollapseProgress(raw)
	if strings.Contains(out, "\r") {
		t.Error("carriage return left")
	}
	if n := strings.Count(out, "Testing DataLoader 0: 100%"); n != 1 {
		t.Errorf("%d bar lines, want 1:\n%s", n, out)
	}
	for _, want := range []string{"MulticlassAccuracy", "Training ended:"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lost", want)
		}
	}
	if n := utf8.RuneCountInString(out); n >= 900 {
		t.Errorf("%d runes, want fewer than 900", n)
	}
}

func TestCollapseProgressRuns(t *testing.T) {
	bar := func(n string) string { return "Epoch " + n + ": 100%|##########| 17/17 [00:03<00:00,  4.57it/s]" }
	cases := []struct{ name, in, want string }{
		{"carriage return keeps the last state", "a\rb\rc\n", "c"},
		{"trailing carriage return", "x\ry\r\n", "y"},
		{"two bars in a row", bar("1") + "\n" + bar("2") + "\n", bar("2")},
		{"bar, text, bar", bar("1") + "\nmid\n" + bar("2"), bar("1") + "\nmid\n" + bar("2")},
		{"bar without a total", "Validation: 0/? [00:00<?, ?it/s]\nVal: 3/? [00:01<?, ?it/s]", "Val: 3/? [00:01<?, ?it/s]"},
		{"zero iterations", "0it [00:00, ?it/s]\n1it [00:01, 1.00it/s]", "1it [00:01, 1.00it/s]"},
		{"traceback is no bar", "Traceback (most recent call last):\nValueError: x\nValueError: y", "Traceback (most recent call last):\nValueError: x\nValueError: y"},
		{"table row is no bar", "| a | 1/2 |\n| b | 3/4 |", "| a | 1/2 |\n| b | 3/4 |"},
		{"timing is no bar", "real\t0m18.620s\nuser\t0m25.695s", "real\t0m18.620s\nuser\t0m25.695s"},
		{"ansi removed", "\x1b[31mred\x1b[0m\x1b[2K", "red"},
		{"crlf like lf", "a\r\nb\r\n", "a\nb\n"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		got := CollapseProgress(c.in)
		if got != strings.TrimRight(c.want, "\n") && got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if again := CollapseProgress(got); again != got {
			t.Errorf("%s: not idempotent: %q then %q", c.name, got, again)
		}
	}
}

func TestTailCollapses(t *testing.T) {
	raw := readTestdata(t, "lightning-test-tail.log")
	if got, want := Tail(raw), Tail(CollapseProgress(raw)); got != want {
		t.Errorf("Tail differs from Tail(CollapseProgress):\n%q\n%q", got, want)
	}
	if strings.Contains(Tail(raw), "\r") {
		t.Error("Tail keeps carriage returns")
	}
}

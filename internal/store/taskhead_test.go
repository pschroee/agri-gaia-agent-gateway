package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The task head of the short run list (issue #60) keeps at most TaskHeadLines lines and TaskHeadBytes bytes and never
// cuts a character in half.
func TestTaskHead(t *testing.T) {
	if got := taskHead("## Task\nList the models\n"); got != "## Task\nList the models" {
		t.Fatalf("short task: %q", got)
	}
	if got := taskHead(strings.Repeat("a\n", 20)); strings.Count(got, "\n") != TaskHeadLines-1 {
		t.Fatalf("lines: %q", got)
	}
	got := taskHead(strings.Repeat("ä", 400))
	if len(got) > TaskHeadBytes || !utf8.ValidString(got) {
		t.Fatalf("bytes: %d valid %v", len(got), utf8.ValidString(got))
	}
	if taskHead("") != "" {
		t.Fatal("empty task")
	}
}

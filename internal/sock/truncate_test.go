// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/execproto"
)

// X5.4: a command at the limit is stored in full and not marked.
func TestShortCommandNotMarked(t *testing.T) {
	cmd := strings.Repeat("a", maxLoggedCommand)
	args, meta := summarize(execproto.Request{Op: execproto.OpBash, Command: cmd, Cwd: "/workspace"})
	if meta.Truncated || meta.Bytes != int64(len(cmd)) {
		t.Fatalf("meta: %+v", meta)
	}
	var m map[string]any
	_ = json.Unmarshal(args, &m)
	if m["command"] != cmd {
		t.Fatalf("command shortened: %d bytes", len(m["command"].(string)))
	}
}

// X5.4: beyond the limit the row says so and keeps length and checksum of the untruncated command.
func TestCommandTruncationMarked(t *testing.T) {
	cmd := strings.Repeat("b", maxLoggedCommand+10)
	args, meta := summarize(execproto.Request{Op: execproto.OpBash, Command: cmd})
	sum := sha256.Sum256([]byte(cmd))
	if !meta.Truncated || meta.Bytes != int64(len(cmd)) || meta.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("meta: %+v", meta)
	}
	var m map[string]any
	_ = json.Unmarshal(args, &m)
	s, _ := m["command"].(string)
	if !strings.HasSuffix(s, "… [truncated]") {
		t.Fatalf("truncation not marked: %q", s)
	}
}

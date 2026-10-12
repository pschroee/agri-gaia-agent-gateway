// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import "testing"

// The retention of the trash is off unless it is set (issue #76): unset, empty, 0 and negative values keep chats.
func TestTrashRetentionDays(t *testing.T) {
	for _, c := range []struct {
		val  string
		want int
	}{{"", 0}, {"0", 0}, {"-3", 0}, {"30", 30}} {
		t.Setenv("AGW_TRASH_RETENTION_DAYS", c.val)
		if got := FromEnv().TrashRetentionDays; got != c.want {
			t.Errorf("AGW_TRASH_RETENTION_DAYS=%q: %d, want %d", c.val, got, c.want)
		}
	}
}

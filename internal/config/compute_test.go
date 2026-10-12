// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import "testing"

func TestComputeMaxPerChat(t *testing.T) {
	cases := map[string]int{
		"":   3,
		"5":  5,
		"1":  1,
		"0":  1,
		"-2": 1,
		"x":  3,
	}
	for v, want := range cases {
		t.Setenv("AGW_COMPUTE_MAX_PER_CHAT", v)
		if got := FromEnv().ComputeMaxPerChat; got != want {
			t.Errorf("AGW_COMPUTE_MAX_PER_CHAT=%q: got %d, want %d", v, got, want)
		}
	}
}

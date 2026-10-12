// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"testing"
	"time"
)

func TestPlatformTokenMaxAge(t *testing.T) {
	t.Setenv("AGW_PLATFORM_TOKEN_MAX_AGE", "")
	if got := FromEnv().PlatformTokenMaxAge; got != 5*time.Minute {
		t.Fatalf("default: %v", got)
	}
	t.Setenv("AGW_PLATFORM_TOKEN_MAX_AGE", "2m")
	if got := FromEnv().PlatformTokenMaxAge; got != 2*time.Minute {
		t.Fatalf("2m: %v", got)
	}
}

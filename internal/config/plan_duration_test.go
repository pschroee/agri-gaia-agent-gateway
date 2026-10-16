// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"testing"
	"time"
)

// A-E3 (D3.3): a plan must not run into the end of the login session, whose longest life is 12 h
// (oidc.MaxSession). AGW_PLAN_MAX_DURATION is therefore clamped to PlanDurationCap (11 h), whatever the
// environment asks for; the default stays below it.
func TestPlanMaxDurationCapped(t *testing.T) {
	t.Setenv("AGW_PLAN_MAX_DURATION", "")
	if got := FromEnv().PlanMaxDuration; got != DefaultPlanMaxDuration {
		t.Fatalf("default: %v, want %v", got, DefaultPlanMaxDuration)
	}
	if PlanDurationCap != 11*time.Hour {
		t.Fatalf("PlanDurationCap = %v, want 11 h (oidc.MaxSession 12 h minus one hour of headroom)", PlanDurationCap)
	}
	t.Setenv("AGW_PLAN_MAX_DURATION", "12h")
	if got := FromEnv().PlanMaxDuration; got != PlanDurationCap {
		t.Fatalf("12h: %v, want the cap %v", got, PlanDurationCap)
	}
	t.Setenv("AGW_PLAN_MAX_DURATION", "11h")
	if got := FromEnv().PlanMaxDuration; got != PlanDurationCap {
		t.Fatalf("11h: %v, want %v", got, PlanDurationCap)
	}
	t.Setenv("AGW_PLAN_MAX_DURATION", "2h")
	if got := FromEnv().PlanMaxDuration; got != 2*time.Hour {
		t.Fatalf("2h: %v, want 2h", got)
	}
}

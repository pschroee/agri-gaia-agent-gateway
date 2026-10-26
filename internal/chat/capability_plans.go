// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"time"

	"agw/internal/plan"
)

// The capability "plans" (issue #218): what a plan may ask for. The limits of an autonomous plan's budget come
// from the environment (AGW_PLAN_MAX_*), so the agent cannot read them from the constant description of
// propose_plan; this entry names them, and the description points at it.

func init() { registerCapability("plans", capabilityPlans, setupPlans) }

// plansWithout is what the agent does when autonomous plans are not configured.
const plansWithout = "propose interactive plans only: the user approves the plan once and each matching call then runs"

func capabilityPlans(_ context.Context, m *Manager, _ string) Capability {
	return plansEntry(m.opt.PlanMaxDuration, m.opt.PlanMaxLLMCost)
}

func setupPlans(s Setup) Capability {
	return plansEntry(s.Env.PlanMaxDuration, s.Env.PlanMaxLLMCost)
}

// plansEntry describes the limits of a plan; without limits an autonomous plan is not offered.
func plansEntry(maxDuration time.Duration, maxCost float64) Capability {
	if maxDuration <= 0 || maxCost <= 0 {
		return Capability{
			Detail:  fmt.Sprintf("autonomous plans are not configured on this instance; interactive plans up to %d steps", plan.MaxSteps),
			Without: plansWithout,
		}
	}
	return Capability{
		Available: true,
		Detail: fmt.Sprintf("up to %d steps; autonomous budget at most %d minutes, %.2f USD and %d turns without the user; max_attempts %d-%d",
			plan.MaxSteps, int(maxDuration/time.Minute), maxCost, plan.MaxAutoTurnsCap, plan.MinPlanAttempts, plan.MaxPlanAttempts),
		Without: plansWithout,
	}
}

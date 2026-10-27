// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/store"
	"agw/internal/watch"
)

// The lines the gateway adds to the notes at the end of a training or a search (issue #248): the agent shows
// the learning curves unasked (skill charts), and it tells the user when a run used the CPU although the
// platform is meant to have a GPU.

// cpuRunWarning is the sentence of a run that sampled no GPU while AGW_PLATFORM_GPU says the platform has one.
const cpuRunWarning = "This run used the CPU although the platform has a GPU; tell the user (the operator should check the backend's GPU)."

// curvesLine asks for the learning curves of one training container.
func curvesLine(containerRef string) string {
	return "Show the learning curves (skill charts, data from {{tool:training_curves}} with train_container_id=" + containerRef + ")."
}

// curvesLineSearch asks for the curves of every trial of a search.
func curvesLineSearch(searchID string) string {
	return "Show the learning curves of the trials (skill charts, data from {{tool:training_curves}} with search_id=" + searchID + ")."
}

// cpuRunLine is the warning when the trial sampled no GPU although the platform should have one; else "".
// A trial whose GPU use is still unknown (nil) gets none.
func (m *Manager) cpuRunLine(t store.Trial) string {
	if m.opt.PlatformGPU != "expected" || t.GPUAvailable == nil || *t.GPUAvailable {
		return ""
	}
	return cpuRunWarning
}

// trainingEndLines are the lines for the note of a finished training run outside a search: the curves, and the
// CPU warning. Empty for other kinds of jobs, for a failed run and for the trials of a search (their notes
// are the search's).
func (m *Manager) trainingEndLines(ctx context.Context, j store.WatchJob, state string) string {
	if j.Kind != trialKindTraining || state == watch.StateFailed || j.Ref == "" {
		return ""
	}
	t, err := m.st.TrialByJob(ctx, j.ID)
	if err != nil || t.SearchID != "" {
		return ""
	}
	out := curvesLine(j.Ref)
	if w := m.cpuRunLine(t); w != "" {
		out += " " + w
	}
	return out
}

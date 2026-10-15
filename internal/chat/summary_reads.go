// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"agw/internal/platform"
)

// The gateway reads platform state to write what an approval says will happen (platform.Summarize / SummarizePlan,
// issue #72). These reads belong to the system, never to the agent, but they are visible: one activity entry per
// summary (op summary_read, via orchestrator, issue L2/#119). The entry is outside the figures of the activity:
// activitySummary counts op = 'platform' only, so the key numbers of the evaluation stay about the agent.

// readLog counts what a loggedSummaryReader read: the paths in the order they were read and how many reads failed.
type readLog struct {
	mu     sync.Mutex
	paths  []string
	failed int
}

// maxSummaryReadPaths is how many paths the detail of a summary_read entry names; the rest is summarized as a count.
const maxSummaryReadPaths = 20

// failedReads returns how many of the logged reads failed.
func (rl *readLog) failedReads() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.failed
}

// loggedSummaryReader is summaryReader with a log (issue L2/#119): every reading GET goes through unchanged, and
// path and failure are counted for the activity entry. A read counts as failed when the platform call returns an
// error, reports status "error" or answers 4xx or 5xx.
func (m *Manager) loggedSummaryReader(chatID string, rl *readLog) platform.Doer {
	read := m.summaryReader(chatID)
	if read == nil {
		return nil
	}
	return func(ctx context.Context, r platform.Request) (platform.Result, error) {
		res, err := read(ctx, r)
		rl.mu.Lock()
		rl.paths = append(rl.paths, r.Method+" "+r.Path)
		if err != nil || res.Status == "error" || res.HTTPStatus >= 400 {
			rl.failed++
		}
		rl.mu.Unlock()
		return res, err
	}
}

// logSummaryReads writes the one activity entry of a summary's reads (DL2.5). Nothing is logged when no read
// happened (binding without a platform, or a summary that names no object). phase is "proposed" for the reads that
// wrote the summary at proposal time and "approval" for the re-read at the moment of the decision.
func (m *Manager) logSummaryReads(ctx context.Context, chatID, approvalID, phase string, rl *readLog) {
	rl.mu.Lock()
	paths, failed := append([]string(nil), rl.paths...), rl.failed
	rl.mu.Unlock()
	if len(paths) == 0 {
		return
	}
	shown, tail := paths, ""
	if len(shown) > maxSummaryReadPaths {
		tail = fmt.Sprintf(", … and %d more", len(shown)-maxSummaryReadPaths)
		shown = shown[:maxSummaryReadPaths]
	}
	detail := fmt.Sprintf("for approval %s (%s): %s%s", shortApprovalID(approvalID), phase, strings.Join(shown, ", "), tail)
	result := "ok"
	if failed > 0 {
		result = fmt.Sprintf("error: %d of %d not readable", failed, len(paths))
	}
	m.LogCallBy(ctx, m.slotOf(chatID), chatID, "orchestrator", "summary_read", detail, result)
}

// shortApprovalID is the first eight characters of an approval id, as the entry names it.
func shortApprovalID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

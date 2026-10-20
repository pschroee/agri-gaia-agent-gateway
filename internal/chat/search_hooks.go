// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/store"
	"agw/internal/watch"
)

// The seams of the training search (issue #168) in the job watcher. Other features register what they want to
// know about a watched job here, from an init() in their own file, so that pollJob and finishWatched stay as they
// are: a hook runs for every job the watcher reads, inside a plan or outside, and must find out for itself
// whether the job is its business (by kind, by ref, by trial). A hook does not block the poll: it returns fast
// and keeps slow work to itself.

// jobHook is called with the row of the job as the poll left it and the progress the kind reported.
type jobHook func(ctx context.Context, m *Manager, j store.WatchJob, p watch.Progress)

var (
	jobProgressHooks []jobHook
	jobEndedHooks    []jobHook
)

// registerJobProgress registers a hook for every poll that stored a progress (state running or ended).
func registerJobProgress(h jobHook) { jobProgressHooks = append(jobProgressHooks, h) }

// registerJobEnded registers a hook for the end of a job (done, failed or no longer readable), before the plan
// step of the job, if any, is finished.
func registerJobEnded(h jobHook) { jobEndedHooks = append(jobEndedHooks, h) }

func (m *Manager) runJobProgress(ctx context.Context, j store.WatchJob, p watch.Progress) {
	for _, h := range jobProgressHooks {
		h(ctx, m, j, p)
	}
}

func (m *Manager) runJobEnded(ctx context.Context, j store.WatchJob, p watch.Progress) {
	for _, h := range jobEndedHooks {
		h(ctx, m, j, p)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/store"
)

// The rows of pre-annotation are read here (V4, issue #112): the API's annotation route shows the runs
// and jobs of a chat with their results, and the card must load them after a reload of a dormant chat.
// Reading never wakes a chat and takes no slot; the service of V1/V3 reads the same tables directly.

// AnnotationRuns returns a chat's pre-annotation runs, oldest first.
func (m *Manager) AnnotationRuns(ctx context.Context, chatID string) ([]store.AnnotationRun, error) {
	return m.st.AnnotationRuns(ctx, chatID)
}

// AnnotationJobs returns a chat's pre-annotation jobs, oldest first.
func (m *Manager) AnnotationJobs(ctx context.Context, chatID string) ([]store.AnnotationJob, error) {
	return m.st.AnnotationJobs(ctx, chatID)
}

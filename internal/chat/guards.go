// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"sync"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// Guards check a platform call that is otherwise allowed (G1.4). They run after the delegation of the
// chat has been checked and before the compute limit, the plan match and the approval — also in
// PlatformPrecheck, so that a refused call reads no files. Features register their own guards from an
// init() in their own file (plan delegation A-E1, edge guard D-E1).
type guard func(ctx context.Context, m *Manager, chatID string, req platform.Request, a delegation.Access) (refusal string, err error)

var (
	guardMu sync.Mutex
	guards  []guard
)

// registerGuard registers a guard (G1.4). A refusal (non-empty string) answers the call with "denied"
// and that reason; an error fails closed, so the call does not go out.
func registerGuard(g guard) {
	guardMu.Lock()
	defer guardMu.Unlock()
	guards = append(guards, g)
}

// guardRefusal runs the guards in the order they were registered. refused true carries the answer to
// hand back; the error fails closed.
func (m *Manager) guardRefusal(ctx context.Context, chatID string, req platform.Request, a delegation.Access) (res platform.Result, refused bool, err error) {
	guardMu.Lock()
	list := append([]guard(nil), guards...)
	guardMu.Unlock()
	for _, g := range list {
		refusal, err := g(ctx, m, chatID, req, a)
		if err != nil {
			return platform.Result{}, false, fmt.Errorf("guard: %w", err)
		}
		if refusal != "" {
			return platform.Result{Status: "denied", Message: platform.RefusedPrefix + refusal}, true, nil
		}
	}
	return platform.Result{}, false, nil
}

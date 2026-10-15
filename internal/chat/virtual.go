// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"

	"agw/internal/platform"
)

// VirtualDoer answers the calls under its own path prefix itself, instead of the platform (G1.5):
// the notebook tools (plan "own code" C-E3) and the pre-annotation (V1) work this way. Everything
// else under /_agw/ that nobody answers is reserved for the gateway and never sent to the platform.
type VirtualDoer interface {
	Do(ctx context.Context, chatID string, req platform.Request) (platform.Result, error)
}

// RegisterVirtual registers a doer for a path prefix (G1.5), e.g. "/_agw/notebook": a call whose path
// is the prefix or starts with the prefix plus a slash goes there. The longest matching prefix wins.
// Because the routes of G1 carry no confirm-each yet, registration is what makes such a path reachable
// at all.
func (m *Manager) RegisterVirtual(prefix string, d VirtualDoer) {
	if m == nil || d == nil || !strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.virtuals == nil {
		m.virtuals = map[string]VirtualDoer{}
	}
	m.virtuals[prefix] = d
}

// virtualFor finds the doer of a path (longest prefix wins).
func (m *Manager) virtualFor(path string) (VirtualDoer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best VirtualDoer
	bestLen := 0
	for prefix, d := range m.virtuals {
		if !pathMatchesPrefix(path, prefix) || len(prefix) <= bestLen {
			continue
		}
		best, bestLen = d, len(prefix)
	}
	return best, best != nil
}

// pathMatchesPrefix reports whether path is the prefix or lies below it.
func pathMatchesPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// reservedVirtual names the prefix of a path under /_agw/ that no doer answers, e.g. "/_agw/notebook"
// for "/_agw/notebook/runs".
func reservedVirtual(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/_agw/")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return "", false
	}
	return "/_agw/" + name, true
}

// virtualUnavailable answers a /_agw/ path that no doer registered (G1.5): the prefix belongs to the
// gateway and never goes to the platform. It is answered before the guards and before any approval or
// compute call, because there is nothing to decide about (G1.13).
func (m *Manager) virtualUnavailable(path string) (platform.Result, bool) {
	prefix, reserved := reservedVirtual(path)
	if !reserved {
		return platform.Result{}, false
	}
	if _, ok := m.virtualFor(path); ok {
		return platform.Result{}, false
	}
	return platform.Result{Status: "unavailable", Message: prefix + " is not configured on this instance"}, true
}

// platformDo sends a call out: a registered virtual path goes to its doer, an unregistered /_agw/ path
// is answered by the gateway itself (G1.5), everything else goes to the platform.
func (m *Manager) platformDo(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	if d, ok := m.virtualFor(req.Path); ok {
		return d.Do(ctx, chatID, req)
	}
	if res, unavailable := m.virtualUnavailable(req.Path); unavailable {
		return res, nil
	}
	return m.opt.Platform.Do(ctx, chatID, req)
}

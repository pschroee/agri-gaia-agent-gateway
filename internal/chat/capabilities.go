// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"agw/internal/config"
	"agw/internal/platform"
)

// Capability overview (G1.8): what this setup offers right now, derived by the gateway from its
// configuration and state — never from what the agent claims. The model asks for it (the tool
// capabilities, the virtual path platform.CapabilitiesPath) before it plans work that needs one of
// them, and the system note names the parts that do not change during a chat (G1.9).

// Capability is one entry: what it is, whether this setup offers it right now, how it stands, and what
// to do without it.
type Capability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
	Without   string `json:"without"`
}

// Capabilities is the answer of the overview (G1.10): the entries and a short line naming the state,
// so that the agent has both at once. The body stays one JSON object, which keeps the REST binding's
// answer application/json.
type Capabilities struct {
	Summary      string       `json:"summary"`
	Capabilities []Capability `json:"capabilities"`
}

// Setup is everything a capability may look at to describe itself for the system note: the catalog and
// the environment of the orchestrator, and the binding of the slot the note is for (G1.9). It holds no
// chat: whatever changes during a chat belongs into the live entry, not into the note.
type Setup struct {
	Catalog *config.Catalog
	Env     config.Env
	Variant string
}

// knownCapabilities are the names the overview always reports, in the order of the plan (G1.8): vision,
// internet, notebook, edge, annotation, compute, checks, bindings. A name no feature registered is
// reported as not part of this gateway version, so that the model never assumes a capability that is
// not built in (G1.11).
var knownCapabilities = []string{"vision", "internet", "notebook", "edge", "annotation", "compute", "checks", "bindings"}

// missingCapability is what an entry of a feature says that this gateway version does not hold.
const missingCapability = "not part of this gateway version"

var (
	// capabilityFuncs answer a capability for one chat (config and state, no cache of their own).
	capabilityFuncs = map[string]func(ctx context.Context, m *Manager, chatID string) Capability{}
	// capabilitySetups describe the parts of a capability that do not change during a chat, for the
	// system note (optional; a capability without one is only in the tool's answer).
	capabilitySetups = map[string]func(s Setup) Capability{}
)

// registerCapability registers a capability (G1.6, G1.11). live answers for the current chat. setup is
// optional: it answers without a chat, from catalog, environment and binding alone, and feeds the lines
// of the system note — only what does not change during a chat belongs there (G1.9). Register from an
// init() of the feature's own file.
func registerCapability(name string, live func(ctx context.Context, m *Manager, chatID string) Capability, setup ...func(s Setup) Capability) {
	if live != nil {
		capabilityFuncs[name] = live
	}
	if len(setup) > 0 && setup[0] != nil {
		capabilitySetups[name] = setup[0]
	}
}

// capabilityCacheTTL: for this long the answer of a capability that read platform state is kept in
// memory (G1.11), so that repeated calls do not put a platform call behind every entry.
const capabilityCacheTTL = 60 * time.Second

type capabilityCacheEntry struct {
	c  Capability
	at time.Time
}

// cachedCapability answers a capability from memory, or works it out with f and keeps it for
// capabilityCacheTTL (G1.11). Use it for entries that read platform state (notebook, edge, annotation);
// key names the entry's own state.
func (m *Manager) cachedCapability(chatID, key string, f func() Capability) Capability {
	cacheKey := chatID + "\x00" + key
	now := time.Now()
	m.mu.Lock()
	entry, ok := m.capCache[cacheKey]
	if ok && now.Sub(entry.at) < capabilityCacheTTL {
		m.mu.Unlock()
		return entry.c
	}
	// Drop expired entries of other chats while we are here.
	for k, e := range m.capCache {
		if now.Sub(e.at) >= capabilityCacheTTL {
			delete(m.capCache, k)
		}
	}
	m.mu.Unlock()
	c := f()
	m.mu.Lock()
	if m.capCache == nil {
		m.capCache = map[string]capabilityCacheEntry{}
	}
	m.capCache[cacheKey] = capabilityCacheEntry{c: c, at: now}
	m.mu.Unlock()
	return c
}

// Capabilities is the overview for the tool and the virtual path (G1.8): the known names in the order
// of the plan, each answered by its registered entry or reported as missing, plus a short line.
func (m *Manager) Capabilities(ctx context.Context, chatID string) Capabilities {
	out := Capabilities{Capabilities: make([]Capability, 0, len(knownCapabilities))}
	var line []string
	for _, name := range knownCapabilities {
		f, ok := capabilityFuncs[name]
		if !ok {
			out.Capabilities = append(out.Capabilities, Capability{
				Name: name, Available: false, Detail: missingCapability,
				Without: "work without it or ask the user how to proceed",
			})
			line = append(line, name+": no")
			continue
		}
		c := f(ctx, m, chatID)
		c.Name = name
		if c.Without == "" {
			c.Without = "work without it or ask the user how to proceed"
		}
		out.Capabilities = append(out.Capabilities, c)
		line = append(line, name+": "+yesNo(c.Available))
	}
	out.Summary = "This setup offers " + strings.Join(line, ", ") + "."
	return out
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// capabilitiesResult is the answer of the virtual path CapabilitiesPath (G1.10): the overview as JSON.
func (m *Manager) capabilitiesResult(ctx context.Context, chatID string) (platform.Result, bool) {
	b, err := json.Marshal(m.Capabilities(ctx, chatID))
	if err != nil {
		return platform.Result{Status: "error", Message: "capabilities not readable: " + err.Error()}, false
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: string(b)}, true
}

// SetupLines are the capability lines of the system note (G1.9): one line per registered capability
// that describes itself without a chat. The tool capabilities answers the current state.
func SetupLines(s Setup) []string {
	out := make([]string, 0, len(knownCapabilities))
	for _, name := range knownCapabilities {
		f, ok := capabilitySetups[name]
		if !ok {
			continue
		}
		c := f(s)
		out = append(out, name+": "+c.Detail)
	}
	return out
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package toolset describes which platform bindings a chat gets: any combination of the command
// line (cli), MCP (mcp) and the REST API (api). The gateway fixes the combination for every new chat
// with AGW_TOOLSETS (issue #29); there is no choice per chat any more.
//
// A combination has one canonical key, the bindings in the fixed order cli, mcp, api joined by
// commas ("cli", "cli,api", "cli,mcp,api"). The key is what chats store as their variant and what
// the warm pool is keyed by. The keys of the single bindings are the variant ids used before, so
// chats stored as "cli", "mcp" or "api" resume unchanged; the older id "both" means "cli,mcp".
package toolset

import (
	"fmt"
	"strings"
)

// Binding is one way for the agent to reach the platform and the gateway.
type Binding string

const (
	CLI Binding = "cli" // bash, files, subagents; agw-platform and curl to the socket
	MCP Binding = "mcp" // MCP tools (mcp_*), read/write/ls, no bash
	API Binding = "api" // the HTTP tool platform_http
)

// Order is the canonical order of the bindings (keys, tool lists, pi arguments).
var Order = []Binding{CLI, MCP, API}

// Env is the environment variable that fixes the combination for new chats.
const Env = "AGW_TOOLSETS"

// Default applies when AGW_TOOLSETS is not set: the command line alone, as the default variant
// before issue #29. It is the binding with the widest scope of action (bash, files, subagents);
// mcp and api restrict it and are set on purpose, for example for a measurement.
const Default = "cli"

// legacy maps variant ids stored before issue #29 that are not canonical keys.
var legacy = map[string]Set{"both": {cli: true, mcp: true}}

// Set is a non-empty combination of bindings. The zero value is empty and invalid.
type Set struct{ cli, mcp, api bool }

// Of builds a set from bindings (unknown ones are ignored).
func Of(bs ...Binding) Set {
	var s Set
	for _, b := range bs {
		s = s.with(b)
	}
	return s
}

func (s Set) with(b Binding) Set {
	switch b {
	case CLI:
		s.cli = true
	case MCP:
		s.mcp = true
	case API:
		s.api = true
	}
	return s
}

// Has reports whether the set contains the binding.
func (s Set) Has(b Binding) bool {
	switch b {
	case CLI:
		return s.cli
	case MCP:
		return s.mcp
	case API:
		return s.api
	}
	return false
}

// Empty reports whether no binding is set.
func (s Set) Empty() bool { return !s.cli && !s.mcp && !s.api }

// Bindings lists the bindings in canonical order.
func (s Set) Bindings() []Binding {
	out := make([]Binding, 0, 3)
	for _, b := range Order {
		if s.Has(b) {
			out = append(out, b)
		}
	}
	return out
}

// Key is the canonical key: bindings in canonical order, joined by commas.
func (s Set) Key() string {
	parts := make([]string, 0, 3)
	for _, b := range s.Bindings() {
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ",")
}

func (s Set) String() string { return s.Key() }

// Parse reads the value of AGW_TOOLSETS: a comma-separated list of cli, mcp and api in any order
// and combination. Spaces around entries and upper case are tolerated, a repeated entry counts once.
// An empty value, an empty entry ("cli,,api") or an unknown entry is an error, so that a typo stops
// the start instead of quietly giving every chat other tools.
func Parse(v string) (Set, error) {
	if strings.TrimSpace(v) == "" {
		return Set{}, fmt.Errorf("%s is empty; expected a comma-separated list of cli, mcp and api (default %q)", Env, Default)
	}
	var s Set
	for _, part := range strings.Split(v, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		switch Binding(p) {
		case CLI, MCP, API:
			s = s.with(Binding(p))
		case "":
			return Set{}, fmt.Errorf("%s=%q contains an empty entry; expected a comma-separated list of cli, mcp and api", Env, v)
		default:
			return Set{}, fmt.Errorf("%s=%q: unknown binding %q; allowed are cli, mcp and api", Env, v, strings.TrimSpace(part))
		}
	}
	return s, nil
}

// FromVariant reads the variant stored with a chat: a canonical key, a key in another order or
// the older id "both" (= cli,mcp). Anything else is an error.
func FromVariant(v string) (Set, error) {
	if s, ok := legacy[v]; ok {
		return s, nil
	}
	s, err := Parse(v)
	if err != nil {
		return Set{}, fmt.Errorf("unknown binding variant %q", v)
	}
	return s, nil
}

// Normalize returns the canonical key of a stored variant ("both" → "cli,mcp"), or an error.
func Normalize(v string) (string, error) {
	s, err := FromVariant(v)
	if err != nil {
		return "", err
	}
	return s.Key(), nil
}

// Valid reports whether a stored variant can be resumed.
func Valid(v string) bool {
	_, err := FromVariant(v)
	return err == nil
}

// All lists every non-empty combination, ordered by key length and then canonically.
func All() []Set {
	var out []Set
	for n := 1; n <= 3; n++ {
		for mask := 1; mask < 8; mask++ {
			s := Set{cli: mask&1 != 0, mcp: mask&2 != 0, api: mask&4 != 0}
			if len(s.Bindings()) == n {
				out = append(out, s)
			}
		}
	}
	return out
}

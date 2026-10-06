// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package oidc

import (
	"strings"
	"testing"
)

// safeReturn on its own: same-host absolute paths only, under a prefix also outside it (platform frontend).
func TestSafeReturnRules(t *testing.T) {
	for _, base := range []string{"", "/agent"} {
		s := &Service{base: base}
		home := base + "/"
		accepted := []string{home, home + "?embed=1", home + "#/chats/x", "/ai-agent", "/ai-agent?tab=activity", "/datasets?q=a%20b", "/models/7#x"}
		rejected := []string{
			"", "ai-agent", "//evil.com", "///evil.com", "https://evil.com", "http:/evil.com", "javascript:alert(1)",
			"/\\evil.com", "/\\/evil.com", "%2F%2Fevil.com", "/%2F%2Fevil.com", "/%2f%2fevil.com", "/%5cevil.com", "/%2e%2e/x",
			"/a/../b", "/a/./b", "/a b", "/a\tb", "/a\r\nLocation: x", "/a%0d%0ab", "/a%00", base + "/oidc", base + "/oidc/login",
			base + "/oidc/callback?x=1", "/" + strings.Repeat("a", 512),
		}
		for _, p := range accepted {
			if got := s.safeReturn(p); got != p {
				t.Errorf("base %q: %q refused (got %q)", base, p, got)
			}
		}
		for _, p := range rejected {
			if got := s.safeReturn(p); got != home {
				t.Errorf("base %q: %q accepted (got %q)", base, p, got)
			}
		}
	}
}

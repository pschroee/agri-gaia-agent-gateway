// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"strings"
	"testing"

	"agw/internal/platform"
)

// Every gateway route the delegation classifies is either a path of the REST binding (its Example hits the
// route) or an exception for which the REST binding refuses the call; and every Example hits a route (#199).
func TestRESTIndexCoversGatewayRoutes(t *testing.T) {
	exceptions := map[string]string{ // route pattern fragment -> sample path
		"/_agw/assess/models/": "/_agw/assess/models/5/latest",
		"/_agw/assess/files/":  "/_agw/assess/files/" + strings.Repeat("a", 64) + "/latest",
		"/_agw/notebook":       "/_agw/notebook/runs",
	}
	for _, rt := range routes {
		pat := rt.re.String()
		if !strings.HasPrefix(pat, "^/_agw/") {
			continue
		}
		covered := false
		for _, tool := range platform.Tools {
			c := tool.RESTCall()
			if c != nil && c.Method == rt.method && rt.re.MatchString(c.Example) {
				covered = true
			}
		}
		if covered {
			continue
		}
		refused := false
		for frag, sample := range exceptions {
			if sample != "" && strings.Contains(pat, frag) {
				refused = platform.RESTRefusal(rt.method, sample) != ""
				if !refused {
					t.Errorf("route %s %s: the REST binding must refuse %s", rt.method, pat, sample)
				}
				refused = true
			}
		}
		if !refused {
			t.Errorf("route %s %s has no REST description and no listed exception", rt.method, pat)
		}
	}
	special := map[string]bool{"/_agw/rights": true, "/_agw/capabilities": true, "/_agw/plan": true, "/_agw/paths": true, "/_agw/search": true}
	for _, tool := range platform.Tools {
		c := tool.RESTCall()
		if c == nil || special[c.Example] {
			continue
		}
		hit := false
		for _, rt := range routes {
			if rt.method == c.Method && rt.re.MatchString(c.Example) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("%s: the example %s %s hits no route of the delegation", tool.Name, c.Method, c.Example)
		}
	}
}

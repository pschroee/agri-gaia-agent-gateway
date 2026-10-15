// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"

	"agw/internal/platform"
)

// The conformance cases of an autonomous plan's grant (A-E1): a call inside the running plan's delegated
// rights is let through, a call outside it is refused. The grant is the one such a plan derives from its
// steps (internal/plan/grant.go); the cases show that the check is a real boundary and not a check that
// refuses everything (outline 7.3.1).
func init() {
	registerConformance(
		Case{
			Name: "call inside the running plan's grant", Group: "plan", Allowed: true,
			Rules: planGrantConformance,
			Req:   platform.Request{Method: "PATCH", Path: "/datasets/12", Body: json.RawMessage(`{"name":"new"}`)},
		},
		Case{
			Name: "call outside the running plan's grant", Group: "plan",
			Rules: planGrantConformance,
			Req:   platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"new"}`)},
		},
		Case{
			Name: "create a dataset inside the running plan's grant", Group: "plan", Allowed: true,
			Rules: planGrantConformance,
			Req:   platform.Request{Method: "POST", Path: "/datasets", Form: map[string][]string{"name": {"x"}}},
		},
	)
}

// planGrantConformance is a plan's grant as internal/plan derives it: create datasets, change own ones,
// read everything (D1.2, D1.3). confirm "none" keeps the allowed controls free of an approval: the
// conformance check tests the delegated rights, not the separate question whether a write asks.
const planGrantConformance = `{
  "rules": [
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["own"]},
    {"action": "read", "resource": "dataset", "ids": ["*"]}
  ],
  "confirm": "none",
  "enforce": true
}`

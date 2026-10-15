// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import "net/http"

// CapabilitiesPath is the path under which the orchestrator answers the overview of what this setup
// offers (G1.10). Like RightsPath it is answered inside the gateway and never goes to the platform; it
// is reading, needs no approval and is allowed in every binding.
const CapabilitiesPath = "/_agw/capabilities"

// capabilitiesTool: the overview of the setup as a tool of the platform binding (G1.9). The description
// is fixed: it says what the entries are and when to ask for them. The wording is the plan's (G1).
var capabilitiesTool = Tool{
	Name:   "capabilities",
	CLI:    "capabilities",
	Desc:   "What this setup offers right now: vision (can the model read images), internet, notebook server, edge devices online, pre-annotation (CVAT), compute calls left, bindings. Call it before planning work that needs one of them; each entry says what to do without it.",
	Params: nil,
	build:  func(Args) (Request, error) { return Request{Method: http.MethodGet, Path: CapabilitiesPath}, nil },
}

func init() { Tools = append(Tools, capabilitiesTool) }

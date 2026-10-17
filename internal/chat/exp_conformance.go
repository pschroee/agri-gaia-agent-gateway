// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"agw/internal/delegation"
	"agw/internal/platform"
)

// Conformance cases for the protection levels of the experiment (X10.2). The levels map to the
// delegation's enforce flag (X0.7): `none` and `instruction` do not enforce, so a violation runs and
// is only logged as `allowed_violation`; `service` enforces and refuses it. The fixed locks
// (blocked paths, field rules, calls the gateway cannot classify) hold in every level (XE1). Each
// case comes with an allowed control, so that a check refusing everything does not pass.

// The delegations of the three levels: reading any dataset is delegated, everything else is not.
const (
	protectionNoneDelegation        = `{"rules":[{"action":"read","resource":"dataset","ids":["*"]}],"enforce":false,"confirm":"none"}`
	protectionInstructionDelegation = protectionNoneDelegation
	protectionServiceDelegation     = `{"rules":[{"action":"read","resource":"dataset","ids":["*"]}],"confirm":"none"}`
)

func init() { delegation.RegisterConformance(protectionConformanceCases()...) }

// protectionConformanceCases are the cases of X10.2.
func protectionConformanceCases() []delegation.Case {
	req := func(m, p string) platform.Request { return platform.Request{Method: m, Path: p} }
	body := func(m, p, b string) platform.Request {
		return platform.Request{Method: m, Path: p, Body: []byte(b)}
	}
	return []delegation.Case{
		// Stage "no protection": no delegation check, but the fixed locks hold (XE1).
		{Name: "none: blocked path stays refused", Req: req("GET", "/urls/basic-auth"), Raw: true,
			Rules: protectionNoneDelegation, Group: "protection none"},
		{Name: "none: field rule stays refused", Req: body("PATCH", "/datasets/5", `{"bucket_name":"x"}`),
			Rules: protectionNoneDelegation, Group: "protection none"},
		{Name: "none: violation runs and is logged", Req: body("PATCH", "/datasets/5", `{"name":"x"}`), Allowed: true, Violation: true,
			Rules: protectionNoneDelegation, Group: "protection none"},
		{Name: "none: allowed control", Req: req("GET", "/datasets"), Allowed: true,
			Rules: protectionNoneDelegation, Group: "protection none"},

		// Stage "instruction in the prompt": the same enforcement as "none" (X0.7), the instruction
		// alone carries the difference and is not part of this check.
		{Name: "instruction: violation runs and is logged", Req: body("PATCH", "/datasets/5", `{"name":"x"}`), Allowed: true, Violation: true,
			Rules: protectionInstructionDelegation, Group: "protection instruction"},
		{Name: "instruction: allowed control", Req: req("GET", "/datasets"), Allowed: true,
			Rules: protectionInstructionDelegation, Group: "protection instruction"},

		// Stage "service": violations are refused.
		{Name: "service: violation is refused", Req: body("PATCH", "/datasets/5", `{"name":"x"}`),
			Rules: protectionServiceDelegation, Group: "protection service"},
		{Name: "service: allowed control", Req: req("GET", "/datasets"), Allowed: true,
			Rules: protectionServiceDelegation, Group: "protection service"},
	}
}

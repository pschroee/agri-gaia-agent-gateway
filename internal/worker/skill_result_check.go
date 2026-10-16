// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

// The skill of the result checks (R0): how to report figures and a verdict the gateway measured. It is
// loaded in the cli binding like the other command skills (registerCLISkill, G1.6); both paths - with
// and without the capability checks - stand in its description, so the agent knows what to do when this
// setup does not check results.
const resultCheckSkill = "/opt/agw/skills/result-check"

func init() { registerCLISkill(resultCheckSkill) }

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

// The skill of the training search (issue #168): one search with a budget instead of single trainings. It is
// loaded in the cli binding like the other command skills; the rules the agent must know before it acts stand
// in its description.
const trainingSearchSkill = "/opt/agw/skills/training-search"

func init() { registerCLISkill(trainingSearchSkill) }

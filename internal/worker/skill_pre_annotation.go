// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

// preAnnotationSkill is the skill for pre-annotation with CVAT (V1). It is registered through
// registerCLISkill (G1.6) and therefore loaded for the cli binding only: it works with the commands of
// agw-platform, which no other binding has.
const preAnnotationSkill = "/opt/agw/skills/pre-annotation"

func init() { registerCLISkill(preAnnotationSkill) }

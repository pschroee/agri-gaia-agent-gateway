// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import "fmt"

// MinImprovement is the smallest gain on the validation images that makes another round worth it
// (R0.8, the same rule as V3c). Below it the loop stops and reports: further rounds would chase noise.
const MinImprovement = 0.02

// minImprovementEpsilon keeps a difference that is meant to be exactly MinImprovement from falling
// short through floating point noise (0.72 - 0.70).
const minImprovementEpsilon = 1e-9

// MayContinue decides whether another training round is worth it (R0.8): prev and cur are the figures
// on the validation images of the last and of the new round, round their number (from 1). ok is false
// when the loop should stop and report; message then names the reason, ready for the agent's reply.
func MayContinue(prev, cur float64, round int) (ok bool, message string) {
	if cur-prev >= MinImprovement-minImprovementEpsilon {
		return true, ""
	}
	return false, fmt.Sprintf("no improvement on the validation images in round %d (%.2f -> %.2f); stop and report", round, prev, cur)
}

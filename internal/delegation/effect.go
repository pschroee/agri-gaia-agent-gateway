// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import "agw/internal/platform"

// Effect is the class of what a platform call does (issue #63). It decides whether the user must
// approve the call and is shown in approvals, the activity and the step details. The classes are
// the same for CLI, MCP and REST, because every binding goes through Classify.
type Effect string

// Effect classes.
const (
	EffectRead         Effect = "read"         // changes nothing, runs without asking
	EffectWrite        Effect = "write"        // changes data on the platform
	EffectCompute      Effect = "compute"      // uses compute time (building or starting a training)
	EffectIrreversible Effect = "irreversible" // cannot be undone (deleting, publishing)
)

// Valid reports whether e is one of the four classes; "" is not.
func (e Effect) Valid() bool {
	switch e {
	case EffectRead, EffectWrite, EffectCompute, EffectIrreversible:
		return true
	}
	return false
}

// effectOfAction is the class of an action when a route names none of its own.
func effectOfAction(action string) Effect {
	switch action {
	case Read:
		return EffectRead
	case Create, Update:
		return EffectWrite
	case Run:
		return EffectCompute
	case Delete:
		return EffectIrreversible
	}
	return ""
}

// Classify classifies a call and sets its effect class. A call whose class cannot be determined
// keeps the empty class; PlatformCall refuses it (fail closed). Writes stays the floor: a call
// that writes (for example a GET that writes in the backend) is never read.
func Classify(req platform.Request) Access {
	return classifyAs(req, false)
}

// ClassifyPartial is Classify for the sample of a plan step whose tool completes the body at run time
// (platform.Tool.HasSteps, e.g. update_model): the completeness of the body (requiredFields) is not
// checked, every allowlist and every refine of the route is. The call that goes out is classified with
// Classify and refused there if it is incomplete (issue #65).
func ClassifyPartial(req platform.Request) Access {
	return classifyAs(req, true)
}

func classifyAs(req platform.Request, partial bool) Access {
	a := classify(req, partial)
	if a.Effect == "" {
		a.Effect = effectOfAction(a.Action)
	}
	if a.Effect == EffectRead && req.Writes() {
		a.Effect = EffectWrite
	}
	return a
}

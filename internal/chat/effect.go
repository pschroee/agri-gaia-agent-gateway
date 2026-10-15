// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agw/internal/config"
	"agw/internal/delegation"
	"agw/internal/platform"
)

// needsApproval decides whether a platform call waits for the user (issue #63, G1.1). Irreversible calls
// always wait, also with confirm "none", and so do calls on a route marked confirm each (the gateway cannot
// check what they run); reading calls never do; every other call waits unless the delegation says confirm
// "none" (no delegation: it waits).
func needsApproval(a delegation.Access, writes bool, del *delegation.Delegation) bool {
	if a.Effect == delegation.EffectIrreversible || a.ConfirmEach {
		return true
	}
	if a.Effect == delegation.EffectRead && !writes {
		return false
	}
	return del == nil || del.ConfirmWrites()
}

// effectRefusal refuses a call whose class could not be determined (fail closed). refused is false
// for the four classes.
func effectRefusal(eff delegation.Effect) (res platform.Result, refused bool) {
	if eff.Valid() {
		return platform.Result{}, false
	}
	return platform.Result{Status: "error", Message: platform.RefusedPrefix + "the gateway could not classify this call; nothing executed"}, true
}

// computeMax is the compute calls per chat the platform may accept (AGW_COMPUTE_MAX_PER_CHAT).
func (m *Manager) computeMax() int {
	if m.opt.ComputeMaxPerChat < 1 {
		return config.DefaultComputeMaxPerChat
	}
	return m.opt.ComputeMaxPerChat
}

// computeLimitResult is the refusal once a chat has used all its compute calls.
func computeLimitResult(limit int) platform.Result {
	return platform.Result{Status: "error", Message: platform.RefusedPrefix + fmt.Sprintf(
		"compute limit reached: this chat has used %d of %d compute calls (AGW_COMPUTE_MAX_PER_CHAT); nothing executed. A new chat has its own limit.", limit, limit)}
}

// computeAllowed checks before the approval whether the chat may still start a compute call. Nobody is
// asked when it may not. allowed false comes with the refusal to return.
func (m *Manager) computeAllowed(ctx context.Context, chatID string) (res platform.Result, allowed bool, err error) {
	n, err := m.st.ComputeCalls(ctx, chatID)
	if err != nil {
		return platform.Result{}, false, err
	}
	if limit := m.computeMax(); n >= limit {
		return computeLimitResult(limit), false, nil
	}
	return platform.Result{}, true, nil
}

// effectLine is the second line of an approval: the class of the call and what it means.
func effectLine(eff delegation.Effect, computeMax int) string {
	switch eff {
	case delegation.EffectRead:
		return "Effect: read"
	case delegation.EffectWrite:
		return "Effect: write (changes data on the platform)"
	case delegation.EffectCompute:
		return fmt.Sprintf("Effect: compute (uses compute time on the platform; at most %d compute calls per chat)", computeMax)
	case delegation.EffectIrreversible:
		return "Effect: irreversible (cannot be undone; every parameter is shown in full below)"
	}
	return "Effect: unknown"
}

// irreversibleNote lists the CVAT login fields of a deleting body (the platform's DELETE carries them),
// so that the user sees what is sent. A value shorter than four characters is marked as a placeholder.
// Empty for every other class and for bodies without these fields.
func irreversibleNote(eff delegation.Effect, body json.RawMessage) string {
	if eff != delegation.EffectIrreversible || len(body) == 0 {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal(body, &fields) != nil {
		return ""
	}
	out := ""
	for _, name := range []string{"key", "csrftoken", "sessionid"} {
		v, ok := fields[name].(string)
		if !ok {
			continue
		}
		if out == "" {
			out = "\n\nCVAT login fields in the body:"
		}
		out += fmt.Sprintf("\nCVAT field %s = %q", name, v)
		if len([]rune(strings.TrimSpace(v))) < 4 {
			out += " (placeholder)"
		}
	}
	return out
}

// effectRules tells the agent the classes and what needs the user's approval.
func effectRules(computeMax int) string {
	return fmt.Sprintf("Effect classes: read calls run without asking; write and compute calls need the user's approval "+
		"(unless the delegation says confirm none); irreversible calls (deleting, publishing) always need the user's approval; "+
		"at most %d compute calls (building or starting a training) per chat.", computeMax)
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"encoding/json"

	"agw/internal/store"
)

// Kinds of a claim of a search on a call.
const (
	claimClone    = "clone"    // POST /train/containers/{base}/clone with the merged configuration
	claimStart    = "start"    // POST /train/containers/{ref}/run of the declared trial
	claimNotebook = "notebook" // a notebook run of the approved code
	claimFinal    = "final"    // assess_model final=true for the winner of an ended search
	// claimTakeover is the model takeover of the winner's training container, once per search (issue #244).
	claimTakeover = "takeover"
)

// trialClaim says that a call of the agent is the next call of a trial the running search declared (issue
// #168). Covered means the search covers the call (exactly the announced one); a claim that is not covered still
// binds the call to the search, and its ApprovalData go with the approval the user is asked (a notebook run with
// changed code). The fields are written by searchMatch.
type trialClaim struct {
	SearchID string
	TrialID  string
	Kind     string
	Covered  bool
	// Approval is the structured data of the question to the user when the claim is not covered.
	Approval json.RawMessage
	// NewCode is the code a not covered notebook run would run with; the user's approval makes it the search's code.
	NewCode *store.SearchCode
	// Ref is the training container a start claim belongs to.
	Ref string
}

// ApprovalData is the structured data the approval carries (store.Approval.Search); nil for no claim.
func (c *trialClaim) ApprovalData() json.RawMessage {
	if c == nil {
		return nil
	}
	return c.Approval
}

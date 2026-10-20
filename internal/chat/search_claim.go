// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"

	"agw/internal/platform"
)

// Kinds of a claim of a search on a call.
const (
	claimClone    = "clone"    // POST /train/containers/{base}/clone with the merged configuration
	claimStart    = "start"    // POST /train/containers/{ref}/run of the declared trial
	claimNotebook = "notebook" // a notebook run of the approved code
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
}

// ApprovalData is the structured data the approval carries (store.Approval.Search); nil for no claim.
func (c *trialClaim) ApprovalData() json.RawMessage {
	if c == nil {
		return nil
	}
	return c.Approval
}

// searchMatch returns the claim of the chat's running search on a call, or nil when no search applies. It is
// asked after the plan match and only for a call no plan step claims. The seam answers nil; TS-S fills it
// (internal/chat/search_match.go).
func (m *Manager) searchMatch(ctx context.Context, chatID string, req platform.Request) *trialClaim {
	_, _, _ = ctx, chatID, req
	return nil
}

// searchFinish records the outcome of a claimed call (the clone made, the trial started, the run begun). It is
// called after the platform answered, with the answer or the error. A nil claim does nothing. The seam does
// nothing; TS-S fills it.
func (m *Manager) searchFinish(ctx context.Context, chatID string, c *trialClaim, req platform.Request, res platform.Result, callErr error) {
	_, _, _, _, _, _ = ctx, chatID, c, req, res, callErr
}

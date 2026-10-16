// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package assess checks the result of a long step: the gateway runs the model in a sealed container of
// its own, measures the raw predictions against pictures or test requests it holds back, and works out
// the figures, a baseline and the verdict in Go (P5). Training (R1), own code (R2), an edge deployment
// (R3) and the pre-annotation (V3b–V3d) use the same types, figures and report form; the agent reports
// the result, it never computes or judges it itself.
package assess

// Verdict is what the gateway thinks of a result. The agent starts its reply with the header that
// carries it and never calls a result done without "usable".
type Verdict string

const (
	// Usable: the figures carry the verdict (the model beats the baseline clearly).
	Usable = Verdict("usable")
	// Questionable: the result stands, but something is off (weak figures, unclear class order).
	Questionable = Verdict("questionable")
	// NotMeasured: nothing was measured (too few held-out images, no known origin, not a
	// classification output).
	NotMeasured = Verdict("not_measured")
)

// The sets a check measures on (P3): the hold-out set decides, the validation set only compares rounds.
const (
	// SetHoldout: the images held back for the verdict (about two thirds of the held-out images).
	SetHoldout = "holdout"
	// SetValidation: the images held back for the comparison of rounds (about one third).
	SetValidation = "validation"
	// SetSynthetic: test requests sent to a deployment (R3).
	SetSynthetic = "synthetic"
)

// Report is one check's answer. It is the same shell the pre-annotation's verify answer uses, so that
// the check card and the skill read every check alike (R0.1).
type Report struct {
	Kind string `json:"kind"`
	// Verdict: usable | questionable | not_measured. Empty in a partial report of a failed check.
	Verdict Verdict `json:"verdict"`
	// Final: this check measured at the hold-out set; false means it compared rounds (validation set).
	Final bool `json:"final"`
	// Set: what was measured (holdout | validation | synthetic).
	Set string `json:"set"`
	// Measured: the number of images or test requests behind the figures.
	Measured    int         `json:"measured_images"`
	ImageReview ImageReview `json:"image_review"`
	// Reasons: why the verdict is what it is, from the gateway; the first one stands in the header of a
	// not measured report (R0.2).
	Reasons     []string     `json:"reasons,omitempty"`
	Hints       []string     `json:"hints,omitempty"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	// Figures: what was measured, e.g. Figures of ClassMetrics. nil: nothing was measured.
	Figures any    `json:"figures,omitempty"`
	Note    string `json:"note,omitempty"`
	// Header: the one line the agent starts its reply with (Header(r)).
	Header string `json:"header"`
}

// Suggestion is what the agent may do about a questionable result; the gateway names it, the agent
// follows it within the compute calls that are left (P15).
type Suggestion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ImageReview is whether a text model looked at the pictures of the measured set (P14). A check of the
// gateway (R0–R3) never sends images on its own: it reports Done false with a reason.
type ImageReview struct {
	Done   bool   `json:"done"`
	Model  string `json:"model,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// NewReport fills the shell of a report: kind, set and sample size. The verdict, the reasons and the
// suggestions come from what was measured; Finish writes the header once they stand.
func NewReport(kind, set string, measured int) Report {
	return Report{Kind: kind, Set: set, Measured: measured}
}

// Finish writes the header of the report (Header) into it. Call it once verdict, reasons and image
// review stand; without a verdict the header stays empty.
func (r *Report) Finish() {
	r.Header = Header(*r)
}

// MinHoldoutImages is the smallest hold-out set the gateway measures a final check on. Below it the
// verdict is not_measured: with a handful of pictures no figure says anything (R1; the card shows the
// line of TooFewHoldout).
const MinHoldoutImages = 20

// TooFewHoldout is the reason of a not measured report with too few held-out images. The reasons begin
// small: the head of the report puts the first one right after the dash (W3).
func TooFewHoldout(n int) string {
	return "too few held-out images (" + itoa(n) + " of at least " + itoa(MinHoldoutImages) + ")"
}

// itoa is strconv.Itoa without the import (keeps this file free of it).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

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
	// HoldoutNotRun: a final check ended before the hold-out images ran through the model, because the
	// class order was not derivable from the seen images (#181). The final test stays available.
	HoldoutNotRun bool `json:"holdout_not_run,omitempty"`
	// Header: the one line the agent starts its reply with (Header(r)).
	Header string `json:"header"`
}

// Suggestion is what the agent may do about a questionable result; the gateway names it, the agent
// follows it within the compute calls that are left (P15).
type Suggestion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ImageReview is whether a model looked at the pictures behind the measured task (P14). A check of the
// gateway that never sends images of its own (R0–R3) reports Done false with a reason; the pre-annotation
// (V3d) fills Checked and FlaggedShare as well, and Done only from ten checked images on. The training
// check (R4) fills Of, Checked and Flagged from the misclassified validation images it looked at.
type ImageReview struct {
	Done  bool   `json:"done"`
	Model string `json:"model,omitempty"`
	// Of is how many misclassified validation images the review should look at (R4); 0 otherwise.
	Of int `json:"of,omitempty"`
	// Checked is how many images the review gave a verdict for (V3d, R4); 0 when no model looked at any.
	Checked int `json:"checked,omitempty"`
	// Flagged is how many checked images look mislabelled (R4: the verdict label_wrong). It never changes
	// the verdict of the check, only the hint and the suggestion fix_labels.
	Flagged int `json:"flagged,omitempty"`
	// FlaggedShare is the share of the checked images the review complained about (V3d): above 0.20 the
	// verdict may not be usable, however good the figures are.
	FlaggedShare float64 `json:"flagged_share,omitempty"`
	// Reason says why no model looked at the images; empty when one did.
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

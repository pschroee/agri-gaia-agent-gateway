// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import "strings"

// Header is the one line the agent starts its report with (R0.2). It names the verdict and, behind it
// in brackets, what was measured, so that the user sees at once whether the figure comes from held-back
// images, from the validation set of a round or from test requests to a deployment.
//
//	Result: usable (held-out images, statistics only, no image review)
//	Result: questionable (validation images, interim)
//	Result: usable (synthetic test requests)
//	Result: not measured – too few held-out images (12 of at least 20)
//
// A not measured report carries no brackets: there is nothing measured to name.
func Header(r Report) string {
	if r.Verdict == "" {
		// A partial report of a failed check: there is no verdict, so there is no header either (L5).
		return ""
	}
	head := "Result: not measured"
	switch r.Verdict {
	case Usable:
		head = "Result: usable"
	case Questionable:
		head = "Result: questionable"
	case NotMeasured:
		if len(r.Reasons) > 0 && r.Reasons[0] != "" {
			head = "Result: not measured – " + r.Reasons[0]
		}
	}
	if r.Verdict == NotMeasured {
		return head
	}
	return head + " (" + measuredText(r) + ")"
}

// measuredText names in the brackets of the header what was measured (R0.2).
func measuredText(r Report) string {
	parts := make([]string, 0, 3)
	switch r.Set {
	case SetValidation:
		parts = append(parts, "validation images", "interim")
	case SetSynthetic:
		parts = append(parts, "synthetic test requests")
	default:
		parts = append(parts, "held-out images", "statistics only")
	}
	switch {
	case r.ImageReview.Done && r.ImageReview.Model != "":
		parts = append(parts, "image review with "+r.ImageReview.Model)
	case !r.ImageReview.Done && r.Set != SetSynthetic:
		parts = append(parts, "no image review")
	}
	return strings.Join(parts, ", ")
}

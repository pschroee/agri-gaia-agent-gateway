// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import "testing"

// Every head of a report, word for word: the agent takes it over, and the card shows it (R0.2, W3).
func TestHeaderForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  Report
		want string
	}{
		{"usable on held-out images", Report{Verdict: Usable, Set: SetHoldout},
			"Result: usable (held-out images, statistics only, no image review)"},
		{"questionable on the validation images", Report{Verdict: Questionable, Set: SetValidation},
			"Result: questionable (validation images, interim, no image review)"},
		{"synthetic test requests", Report{Verdict: Usable, Set: SetSynthetic},
			"Result: usable (synthetic test requests)"},
		{"with image review", Report{Verdict: Questionable, Set: SetHoldout, ImageReview: ImageReview{Done: true, Model: "local/vision"}},
			"Result: questionable (held-out images, statistics only, image review with local/vision)"},
		{"not measured names the first reason without brackets", Report{Verdict: NotMeasured, Set: SetHoldout,
			Reasons: []string{TooFewHoldout(12), "the origin of the model is unknown"}},
			"Result: not measured – too few held-out images (12 of at least 20)"},
		{"not measured without a reason", Report{Verdict: NotMeasured, Set: SetHoldout},
			"Result: not measured"},
		{"partial report of a failed check", Report{Kind: "training_model", Set: SetHoldout},
			""},
	} {
		if got := Header(tc.rep); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	// Finish writes the head into the report; NewReport leaves it empty until the verdict stands.
	r := NewReport("training_model", SetHoldout, 42)
	if r.Header != "" {
		t.Errorf("a fresh report has a head: %q", r.Header)
	}
	r.Verdict = Usable
	r.Finish()
	if r.Header != Header(r) || r.Header == "" {
		t.Errorf("Header after Finish: %q", r.Header)
	}
	if r.Verdict != Usable || r.Measured != 42 || r.Kind != "training_model" {
		t.Errorf("report: %+v", r)
	}
	if TooFewHoldout(12) != "too few held-out images (12 of at least 20)" {
		t.Errorf("reason: %q", TooFewHoldout(12))
	}
}

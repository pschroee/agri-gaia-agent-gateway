// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"math"
	"testing"
)

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-6 }

// A known table: the figures per class, the confusion table and the class without a prediction.
func TestClassMetrics(t *testing.T) {
	truth := []string{"a", "a", "a", "b", "b", "b", "c", "c", "c"}
	pred := []string{"a", "a", "b", "b", "b", "c", "c", "c", "c"}
	f := ClassMetrics(truth, pred, []string{"a", "b", "c", "d"})
	if f.Images != 9 {
		t.Fatalf("images: %d", f.Images)
	}
	if !closeTo(f.Accuracy, 7.0/9.0) {
		t.Errorf("accuracy: %v, want %v", f.Accuracy, 7.0/9.0)
	}
	// Macro F1 over the four classes of the list: a 0.8, b 2/3, c 6/7, d 0 (never predicted).
	if !closeTo(f.MacroF1, (0.8+2.0/3.0+6.0/7.0)/4) {
		t.Errorf("macro F1: %v", f.MacroF1)
	}
	if f.Confusion["a"]["b"] != 1 || f.Confusion["b"]["c"] != 1 || f.Confusion["c"]["c"] != 3 {
		t.Errorf("confusion: %v", f.Confusion)
	}
	a, ok := f.PerClass["a"]
	if !ok || a.Precision == nil || !closeTo(*a.Precision, 1) || !closeTo(a.Recall, 2.0/3.0) || a.Support != 3 {
		t.Errorf("class a: %+v", a)
	}
	// A class the model never predicted has no precision at all (nil), not a zero.
	if d := f.PerClass["d"]; d.Precision != nil || d.Support != 0 || d.Recall != 0 {
		t.Errorf("class without a prediction: %+v", d)
	}
	if len(f.PerClass["d"].Below) != 0 {
		t.Errorf("class without images names figures below their limit: %v", f.PerClass["d"].Below)
	}
	// Without a class list the values decide, sorted.
	if f2 := ClassMetrics(truth, pred, nil); f2.PerClass["b"].Support != 3 || len(f2.PerClass) != 3 {
		t.Errorf("classes from the values: %+v", f2.PerClass)
	}
}

// A class with at least three images and a hit rate under the limit names exactly "recall"; the check
// card paints only those figures red (L19).
func TestClassMetricsBelowRecall(t *testing.T) {
	truth := []string{"a", "a", "a", "b", "b", "b"}
	pred := []string{"b", "b", "b", "a", "b", "b"}
	f := ClassMetrics(truth, pred, []string{"a", "b"})
	if got := f.PerClass["a"].Below; len(got) != 1 || got[0] != "recall" {
		t.Errorf("class a: below %v (recall %v)", got, f.PerClass["a"].Recall)
	}
	if got := f.PerClass["b"].Below; len(got) != 0 {
		t.Errorf("class b reaches the limit but names %v (recall %v)", got, f.PerClass["b"].Recall)
	}
}

// The majority class on a tie: the alphabetically first one, so that the same data always gives the same
// baseline.
func TestMajorityTie(t *testing.T) {
	b := Majority(map[string]int{"Kuh": 3, "Rind": 3, "Schwein": 1}, []string{"Rind", "Rind", "Kuh", "Rind"})
	if b.Class != "Kuh" {
		t.Fatalf("class: %q", b.Class)
	}
	// The baseline answers the majority class everywhere: three of four images.
	if !closeTo(b.Accuracy, 0.75) {
		t.Errorf("accuracy: %v", b.Accuracy)
	}
	// Macro F1 over the two classes of the measured set: "Kuh" as the only answer reaches precision 0.25
	// and recall 1 (F1 0.4), "Rind" is never predicted (no precision, F1 0).
	if !closeTo(b.MacroF1, 0.2) {
		t.Errorf("macro F1: %v", b.MacroF1)
	}
}

// The binomial tail is exact: the test values of the plan (R0.5).
func TestBinomialTail(t *testing.T) {
	for _, tc := range []struct {
		k, n int
		p    float64
		want float64
	}{
		{22, 24, 0.5, 1.8e-5},
		{13, 20, 0.5, 0.1316},
		{0, 20, 0.5, 1},
		{21, 20, 0.5, 0},
		{20, 20, 0.5, 9.5e-7},
		{10, 10, 0.9, 0.3487},
	} {
		got := BinomialTail(tc.k, tc.n, tc.p)
		if math.Abs(got-tc.want) > math.Max(1e-4, tc.want*0.02) {
			t.Errorf("BinomialTail(%d, %d, %v) = %v, want %v", tc.k, tc.n, tc.p, got, tc.want)
		}
	}
	// The edge cases are exact and do not divide by zero.
	if got := BinomialTail(5, 10, 0); got != 0 {
		t.Errorf("p = 0: %v", got)
	}
	if got := BinomialTail(5, 10, 1); got != 1 {
		t.Errorf("p = 1: %v", got)
	}
}

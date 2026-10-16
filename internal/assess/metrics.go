// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import "sort"

// The limits of the figures per class. They are constants of the gateway so that the work can name
// them, and so that card and report show the same line in red.
const (
	// BelowRecall is the hit rate a class must reach; below it the class is named in Below (the check
	// card paints exactly those figures red, L19).
	BelowRecall = 0.50
	// BelowMinImages is the smallest number of images of a class for which a hit rate says anything.
	BelowMinImages = 3
)

// ClassFigures are the figures of one class.
type ClassFigures struct {
	// Precision: null (nil) when the model never predicted this class; an undefined figure is not zero.
	Precision *float64 `json:"precision"`
	Recall    float64  `json:"recall"`
	F1        float64  `json:"f1"`
	// Support: images of this class in the measured set.
	Support int `json:"support"`
	// Below names the figures of this class under their limit (only "recall" so far); the check card
	// paints exactly these red. Missing: nothing is below its limit (14:56 comment of the issue).
	Below []string `json:"below,omitempty"`
}

// Figures is what the gateway computed from the raw predictions of one check (R0.3, P5).
type Figures struct {
	// Images: pairs the figures rest on (truth and prediction).
	Images   int     `json:"images"`
	Accuracy float64 `json:"accuracy"`
	MacroF1  float64 `json:"macro_f1"`
	// Baseline and BaselineAccuracy: the majority class of the seen data and what it reaches here.
	Baseline         string  `json:"baseline,omitempty"`
	BaselineAccuracy float64 `json:"baseline_accuracy,omitempty"`
	// Confusion: truth -> prediction -> number of images. Only the classes that occur are keys.
	Confusion map[string]map[string]int `json:"confusion"`
	PerClass  map[string]ClassFigures   `json:"per_class"`
}

// ClassMetrics computes the figures of a classification check from the truth and the predictions of the
// measured images (R0.3). classes names the classes of the data; empty: they are taken from the values,
// sorted. The lists are read pairwise; a longer list is cut to the shorter one (there is no pair
// without its counterpart).
func ClassMetrics(truth, pred []string, classes []string) Figures {
	f := Figures{Confusion: map[string]map[string]int{}, PerClass: map[string]ClassFigures{}}
	n := len(truth)
	if len(pred) < n {
		n = len(pred)
	}
	f.Images = n
	if len(classes) == 0 {
		seen := map[string]bool{}
		for i := 0; i < n; i++ {
			seen[truth[i]] = true
			seen[pred[i]] = true
		}
		for c := range seen {
			classes = append(classes, c)
		}
		sort.Strings(classes)
	}
	// The confusion table, filled over the given classes and every value that occurs besides them.
	all := append([]string(nil), classes...)
	known := map[string]bool{}
	for _, c := range classes {
		known[c] = true
	}
	for i := 0; i < n; i++ {
		for _, c := range [2]string{truth[i], pred[i]} {
			if !known[c] {
				known[c] = true
				all = append(all, c)
			}
		}
		if f.Confusion[truth[i]] == nil {
			f.Confusion[truth[i]] = map[string]int{}
		}
		f.Confusion[truth[i]][pred[i]]++
	}
	correct := 0
	for i := 0; i < n; i++ {
		if truth[i] == pred[i] {
			correct++
		}
	}
	if n > 0 {
		f.Accuracy = float64(correct) / float64(n)
	}
	sumF1 := 0.0
	for _, c := range all {
		var tp, support, predicted int
		for i := 0; i < n; i++ {
			if truth[i] == c {
				support++
				if pred[i] == c {
					tp++
				}
			}
			if pred[i] == c {
				predicted++
			}
		}
		cf := ClassFigures{Support: support}
		if predicted > 0 {
			p := float64(tp) / float64(predicted)
			cf.Precision = &p
		}
		if support > 0 {
			cf.Recall = float64(tp) / float64(support)
		}
		if cf.Precision != nil && *cf.Precision+cf.Recall > 0 {
			cf.F1 = 2 * *cf.Precision * cf.Recall / (*cf.Precision + cf.Recall)
		}
		if cf.Support >= BelowMinImages && cf.Recall < BelowRecall {
			cf.Below = []string{"recall"}
		}
		f.PerClass[c] = cf
		sumF1 += cf.F1
	}
	if len(all) > 0 {
		f.MacroF1 = sumF1 / float64(len(all))
	}
	return f
}

// SetBaseline writes the majority baseline into the figures (P10); the report names it next to the
// measured figures.
func (f *Figures) SetBaseline(b Baseline) {
	f.Baseline = b.Class
	f.BaselineAccuracy = b.Accuracy
}

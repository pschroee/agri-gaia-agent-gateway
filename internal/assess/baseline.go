// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"math"
	"sort"
)

// Baseline is the majority class of the seen data applied to the measured images: what a model that
// always answers the most frequent class would reach (P10).
type Baseline struct {
	Class    string  `json:"class"`
	Accuracy float64 `json:"accuracy"`
	MacroF1  float64 `json:"macro_f1"`
}

// Majority picks the most frequent class of the seen data and applies it to the measured images. On a
// tie the alphabetically first class wins, so that the same data always gives the same baseline.
func Majority(seenCounts map[string]int, truth []string) Baseline {
	best := ""
	bestN := -1
	names := make([]string, 0, len(seenCounts))
	for c := range seenCounts {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if seenCounts[c] > bestN {
			best, bestN = c, seenCounts[c]
		}
	}
	pred := make([]string, len(truth))
	for i := range pred {
		pred[i] = best
	}
	f := ClassMetrics(truth, pred, nil)
	return Baseline{Class: best, Accuracy: f.Accuracy, MacroF1: f.MacroF1}
}

// BetterThanBaseline is the one-sided binomial test of P10: is the model better than the baseline class
// with less than five percent probability by chance? It answers the tail probability; smaller is
// better.
func BetterThanBaseline(k, n int, p float64) float64 { return BinomialTail(k, n, p) }

// BinomialTail is P(X >= k) for X ~ Binomial(n, p), computed exactly over the logarithms of the
// binomial coefficients (math.Lgamma), so that it stays usable for the sample sizes of a check
// (n up to 10 000). A normal approximation is not good enough at n = 20 (R0.5).
func BinomialTail(k, n int, p float64) float64 {
	if k <= 0 {
		return 1
	}
	if k > n {
		return 0
	}
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	logP, logQ := math.Log(p), math.Log1p(-p)
	sum := 0.0
	for i := k; i <= n; i++ {
		// log C(n, i) = lgamma(n+1) - lgamma(i+1) - lgamma(n-i+1)
		l := lgamma(float64(n)+1) - lgamma(float64(i)+1) - lgamma(float64(n-i)+1) + float64(i)*logP + float64(n-i)*logQ
		sum += math.Exp(l)
	}
	if sum > 1 {
		return 1
	}
	if sum < 0 {
		return 0
	}
	return sum
}

// lgamma is math.Lgamma without its sign, which a binomial coefficient does not need.
func lgamma(x float64) float64 {
	v, _ := math.Lgamma(x)
	return v
}

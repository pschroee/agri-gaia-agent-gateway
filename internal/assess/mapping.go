// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"fmt"
	"sort"
	"strings"
)

// The limits of DeriveMapping (P9). The order of a model's outputs is not known from the outside (the
// platform does not say it), so the gateway derives it from the seen images and only trusts it when it
// is clear.
const (
	// MinMappingShare is the share of a class an output must carry among the seen images before the
	// output counts as that class.
	MinMappingShare = 0.60
	// MinMappingImages is how many seen images of a class are needed before the mapping is derived at
	// all: with fewer the majority says nothing.
	MinMappingImages = 5
)

// DeriveMapping names the class each output of the model means (P9). outIdx[i] is the number of the
// output with the highest value for the i-th seen image, truth[i] its class. ok is true only when the
// mapping is unambiguous: every output carries one class with a share of at least MinMappingShare,
// every class of the data has at least MinMappingImages seen images, and outputs and classes correspond
// one to one. Otherwise the check is questionable and reason says why (class order unclear).
func DeriveMapping(outIdx []int, truth []string, nOut int) (m map[int]string, ok bool, reason string) {
	n := len(outIdx)
	if len(truth) < n {
		n = len(truth)
	}
	if nOut <= 0 {
		return nil, false, "the model has no outputs"
	}
	counts := map[int]map[string]int{}
	perClass := map[string]int{}
	for i := 0; i < n; i++ {
		if outIdx[i] < 0 || outIdx[i] >= nOut {
			return nil, false, fmt.Sprintf("the model answered output %d, which it does not have (%d outputs)", outIdx[i], nOut)
		}
		if counts[outIdx[i]] == nil {
			counts[outIdx[i]] = map[string]int{}
		}
		counts[outIdx[i]][truth[i]]++
		perClass[truth[i]]++
	}
	classes := make([]string, 0, len(perClass))
	for c := range perClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		if perClass[c] < MinMappingImages {
			return nil, false, fmt.Sprintf("only %d seen images of class %q (at least %d are needed)", perClass[c], c, MinMappingImages)
		}
	}
	out := make(map[int]string, len(counts))
	taken := map[string]int{}
	idx := make([]int, 0, len(counts))
	for i := range counts {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		total, best, bestN := 0, "", -1
		for _, c := range sortedKeys(counts[i]) {
			total += counts[i][c]
			if counts[i][c] > bestN {
				best, bestN = c, counts[i][c]
			}
		}
		share := float64(bestN) / float64(total)
		if share < MinMappingShare {
			return nil, false, fmt.Sprintf("output %d matches class %q only in %.0f%% of the seen images", i, best, share*100)
		}
		if t, dup := taken[best]; dup {
			return nil, false, fmt.Sprintf("outputs %d and %d both match class %q: the outputs do not match the classes one to one", t, i, best)
		}
		taken[best] = i
		out[i] = best
	}
	for _, c := range classes {
		if _, has := taken[c]; !has {
			return nil, false, fmt.Sprintf("no output matches class %q: the outputs do not match the classes one to one", c)
		}
	}
	if len(out) != len(classes) {
		return nil, false, "the outputs do not match the classes one to one"
	}
	return out, true, ""
}

// ClassOrderUnclear is the reason of a questionable report whose class order could not be derived.
func ClassOrderUnclear(reason string) string {
	if reason == "" {
		return "the class order of the model is unclear"
	}
	return "the class order of the model is unclear: " + strings.TrimRight(reason, ".") + "."
}

// sortedKeys returns the keys of a count map in a fixed order (the same data gives the same reason).
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MapPredictions applies a derived mapping to output indices: it answers the class of every index that
// the mapping knows, and ok false as soon as one index is not in it.
func MapPredictions(m map[int]string, outIdx []int) (pred []string, ok bool) {
	pred = make([]string, len(outIdx))
	for i, idx := range outIdx {
		c, has := m[idx]
		if !has {
			return nil, false
		}
		pred[i] = c
	}
	return pred, true
}

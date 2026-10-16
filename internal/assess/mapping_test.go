// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"strings"
	"testing"
)

// seen builds outIdx and truth for n images per class whose model answer is the given output index.
func seen(index map[string]int, perClass int) (outIdx []int, truth []string) {
	for _, c := range []string{"cow", "pig", "sheep"} {
		for i := 0; i < perClass; i++ {
			outIdx = append(outIdx, index[c])
			truth = append(truth, c)
		}
	}
	return outIdx, truth
}

// The outputs of the model are in another order than the classes: the majority of the seen images names
// the right class for every output.
func TestDeriveMappingSwapped(t *testing.T) {
	outIdx, truth := seen(map[string]int{"cow": 2, "pig": 0, "sheep": 1}, 8)
	m, ok, reason := DeriveMapping(outIdx, truth, 3)
	if !ok {
		t.Fatalf("not derived: %s", reason)
	}
	if m[0] != "pig" || m[1] != "sheep" || m[2] != "cow" {
		t.Fatalf("mapping: %v", m)
	}
	pred, ok := MapPredictions(m, []int{2, 0, 1})
	if !ok || pred[0] != "cow" || pred[1] != "pig" || pred[2] != "sheep" {
		t.Fatalf("predictions: %v %v", pred, ok)
	}
}

// An output that carries its class only in 55 percent of the seen images: the order is not clear, and
// the check stays questionable instead of mapping it.
func TestDeriveMappingUnclear(t *testing.T) {
	// Ten cow images answer output 0, twenty mixed ones answer output 1 (11 sheep, 9 pig = 55 %), the
	// rest of the pig images answer output 2.
	var outIdx []int
	var truth []string
	for i := 0; i < 10; i++ {
		outIdx, truth = append(outIdx, 0), append(truth, "cow")
	}
	for i := 0; i < 20; i++ {
		if i < 11 {
			outIdx, truth = append(outIdx, 1), append(truth, "sheep")
			continue
		}
		outIdx, truth = append(outIdx, 1), append(truth, "pig")
	}
	for i := 0; i < 9; i++ {
		outIdx, truth = append(outIdx, 2), append(truth, "pig")
	}
	m, ok, reason := DeriveMapping(outIdx, truth, 3)
	if ok || m != nil || reason == "" {
		t.Fatalf("derived %v (%s)", m, reason)
	}
	if !strings.Contains(reason, "55%") {
		t.Fatalf("the share did not decide: %s", reason)
	}
}

// Two outputs that both carry the same class: no bijection, so no mapping.
func TestDeriveMappingNotBijective(t *testing.T) {
	outIdx, truth := seen(map[string]int{"cow": 1, "pig": 1, "sheep": 1}, 10)
	m, ok, reason := DeriveMapping(outIdx, truth, 3)
	if ok || m != nil || reason == "" {
		t.Fatalf("derived %v (%s)", m, reason)
	}
	// Fewer than five seen images of a class: the majority says nothing.
	small, smallTruth := seen(map[string]int{"cow": 0, "pig": 1, "sheep": 2}, 4)
	if _, ok, reason := DeriveMapping(small, smallTruth, 3); ok || reason == "" {
		t.Fatalf("four images per class passed: %s", reason)
	}
	// An output the model does not have is a mistake, not a mapping.
	if _, ok, reason := DeriveMapping([]int{5}, []string{"cow"}, 3); ok || reason == "" {
		t.Fatalf("output 5 of 3 passed: %s", reason)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import "sort"

// Item is one held-out image: its SHA-256 and the class it belongs to. The SHA-256 is what makes the
// split independent of file names and dataset names (P1).
type Item struct {
	SHA   string
	Class string
}

// Partition splits the held-out images of every class into the validation set and the hold-out set
// (P3): within a class the images are sorted by their SHA-256, and every third position (0, 3, 6 …)
// goes to the validation set, the rest to the hold-out set. The same images always give the same split,
// whatever order they arrive in; the result is sorted (classes alphabetically, within a class by
// SHA-256), so that two calls are comparable.
func Partition(items []Item) (validation, holdout []Item) {
	byClass := map[string][]Item{}
	for _, it := range items {
		byClass[it.Class] = append(byClass[it.Class], it)
	}
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		list := byClass[c]
		sort.Slice(list, func(i, j int) bool { return list[i].SHA < list[j].SHA })
		for i, it := range list {
			if i%3 == 0 {
				validation = append(validation, it)
				continue
			}
			holdout = append(holdout, it)
		}
	}
	return validation, holdout
}

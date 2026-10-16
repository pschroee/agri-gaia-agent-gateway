// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"strings"
	"testing"
)

// The split is fixed and does not depend on the order the images arrive in: per class every third image
// (sorted by SHA-256) is a validation image, the rest is held out.
func TestPartitionDeterministic(t *testing.T) {
	items := []Item{}
	for _, cls := range []string{"cow", "pig"} {
		for _, sha := range []string{"aa", "bb", "cc", "dd", "ee", "ff"} {
			items = append(items, Item{SHA: sha, Class: cls})
		}
	}
	validation, holdout := Partition(items)
	if len(validation) != 4 || len(holdout) != 8 {
		t.Fatalf("one third of twelve: %d validation, %d holdout", len(validation), len(holdout))
	}
	// Two of six per class, the same two every time, also when the input is shuffled.
	shuffled := []Item{items[5], items[7], items[0], items[11], items[2], items[9], items[1], items[4], items[10], items[3], items[6], items[8]}
	v2, h2 := Partition(shuffled)
	if len(v2) != len(validation) || len(h2) != len(holdout) {
		t.Fatalf("shuffled: %d/%d", len(v2), len(h2))
	}
	for i := range validation {
		if v2[i] != validation[i] {
			t.Fatalf("validation %d: %+v vs %+v", i, v2[i], validation[i])
		}
	}
	for i := range holdout {
		if h2[i] != holdout[i] {
			t.Fatalf("holdout %d: %+v vs %+v", i, h2[i], holdout[i])
		}
	}
	// Per class: aa and dd (positions 0 and 3), for both classes.
	var classes []string
	for _, it := range validation {
		classes = append(classes, it.Class+"-"+it.SHA)
	}
	want := []string{"cow-aa", "cow-dd", "pig-aa", "pig-dd"}
	for i, c := range classes {
		if c != want[i] {
			t.Fatalf("validation: %v, want %v", classes, want)
		}
	}
	// A class of one image: it is a validation image, so the hold-out set never sees it alone.
	v, h := Partition([]Item{{SHA: "aa", Class: "cow"}})
	if len(v) != 1 || len(h) != 0 {
		t.Fatalf("one image: %d/%d", len(v), len(h))
	}
}

// Another round only makes sense with a real improvement on the validation images.
func TestMayContinue(t *testing.T) {
	if ok, msg := MayContinue(0.50, 0.519, 3); ok || msg == "" {
		t.Errorf("+0.019 continues: %v %q", ok, msg)
	} else if !strings.Contains(msg, "round 3") || !strings.Contains(msg, "0.50 -> 0.52") {
		t.Errorf("message: %q", msg)
	}
	if ok, msg := MayContinue(0.50, 0.52, 4); !ok || msg != "" {
		t.Errorf("+0.02 stops: %v %q", ok, msg)
	}
	if ok, _ := MayContinue(0.50, 0.501, 5); ok {
		t.Error("+0.001 continues")
	}
	// A figure that fell is no improvement either.
	if ok, msg := MayContinue(0.60, 0.40, 6); ok || msg == "" {
		t.Errorf("worse continues: %v %q", ok, msg)
	}
}

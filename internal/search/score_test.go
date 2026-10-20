// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import "testing"

func line(epoch int, acc float64, saved bool) Epoch {
	return Epoch{Epoch: epoch, Epochs: 3, Values: map[string]float64{"val_acc": acc, "val_loss": 1 - acc}, Saved: saved}
}

func TestScoreLastEpochAndSaved(t *testing.T) {
	obj := Objective{Metric: "val_acc", Mode: ModeMax}
	// The best value is in epoch 2, but the model that is saved is the one of epoch 3.
	v, e, ok := Score(obj, []Epoch{line(1, 0.5, false), line(2, 0.9, false), line(3, 0.8, false)})
	if !ok || v != 0.8 || e != 3 {
		t.Errorf("last epoch: %v %d %v", v, e, ok)
	}
	// A line marked saved wins over the last one.
	v, e, ok = Score(obj, []Epoch{line(1, 0.5, false), line(2, 0.7, true), line(3, 0.8, false)})
	if !ok || v != 0.7 || e != 2 {
		t.Errorf("saved line: %v %d %v", v, e, ok)
	}
	// The last of several saved lines (the run writes the last line again at the end).
	v, e, ok = Score(obj, []Epoch{line(1, 0.5, true), line(2, 0.6, true)})
	if !ok || v != 0.6 || e != 2 {
		t.Errorf("last saved: %v %d %v", v, e, ok)
	}
	if _, _, ok := Score(obj, nil); ok {
		t.Error("no line, no score")
	}
	if _, _, ok := Score(Objective{Metric: "val_f1", Mode: ModeMax}, []Epoch{line(1, 0.5, false)}); ok {
		t.Error("a line without the metric has no score")
	}
}

func TestObjective(t *testing.T) {
	if o, err := ParseObjective(nil); err != nil || o.Metric != "val_acc" || o.Mode != ModeMax {
		t.Errorf("default: %+v %v", o, err)
	}
	if o, err := ParseObjective([]byte(`{"metric":"val_loss"}`)); err != nil || o.Mode != ModeMin {
		t.Errorf("loss: %+v %v", o, err)
	}
	for _, bad := range []string{`{"metric":"train_acc"}`, `{"metric":"val_loss","mode":"max"}`, `{"metric":"val_acc","extra":1}`} {
		if _, err := ParseObjective([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	max, min := Objective{Mode: ModeMax}, Objective{Mode: ModeMin}
	if !max.Better(2, 1) || max.Better(1, 1) || !min.Better(1, 2) || !max.Reached(0.9, 0.9) || min.Reached(0.5, 0.4) {
		t.Error("better / reached")
	}
}

func TestWinnerAndPatience(t *testing.T) {
	obj := Objective{Metric: "val_acc", Mode: ModeMax}
	r := func(idx int, done bool, s float64) Result { return Result{Idx: idx, Done: done, Score: &s} }
	// A failed trial and a running one never win; equal scores go to the earlier trial.
	results := []Result{r(1, true, 0.7), r(2, true, 0.9), r(3, false, 0.99), r(4, true, 0.9), {Idx: 5, Done: true}}
	idx, s, ok := Winner(obj, results)
	if !ok || idx != 2 || s != 0.9 {
		t.Errorf("winner: %d %v %v", idx, s, ok)
	}
	if _, _, ok := Winner(obj, []Result{r(1, false, 0.5)}); ok {
		t.Error("no finished trial, no winner")
	}
	if n := SinceImprovement(obj, results); n != 1 { // trial 4 did not improve on trial 2; 3 and 5 do not count
		t.Errorf("since improvement: %d", n)
	}
	if n := SinceImprovement(obj, []Result{r(1, true, 0.5), r(2, true, 0.6)}); n != 0 {
		t.Errorf("improving: %d", n)
	}
}

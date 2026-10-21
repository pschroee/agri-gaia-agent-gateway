// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import "testing"

func TestPreprocessDiffs(t *testing.T) {
	run := &PreprocessInput{Color: "gray", Size: []int{28, 28}, Scale: ScaleToUnit}
	cases := []struct {
		name      string
		call, run *PreprocessInput
		want      int
	}{
		{"equal", &PreprocessInput{Color: "gray", Size: []int{28, 28}, Scale: ScaleToUnit}, run, 0},
		{"only named fields", &PreprocessInput{Color: "gray"}, run, 0},
		{"call nil", nil, run, 0},
		{"missing file field is the default", &PreprocessInput{Mean: []float64{0.5}}, run, 1},
		{"default scale in file", &PreprocessInput{Scale: ScaleToUnit}, &PreprocessInput{Color: "gray"}, 0},
		{"broadcast mean", &PreprocessInput{Mean: []float64{0.5}}, &PreprocessInput{Mean: []float64{0.5, 0.5, 0.5}}, 0},
		{"single size", &PreprocessInput{Size: []int{28}}, run, 0},
		{"rounded scale", &PreprocessInput{Scale: 0.00392}, run, 0},
		{"other mean", &PreprocessInput{Mean: []float64{0.13}}, &PreprocessInput{Mean: []float64{0.1307}}, 1},
		{"color and layout", &PreprocessInput{Color: "rgb", Layout: "nhwc"}, run, 2},
	}
	for _, c := range cases {
		if got := preprocessDiffs(c.call, c.run); len(got) != c.want {
			t.Errorf("%s: %v (want %d)", c.name, got, c.want)
		}
	}
}

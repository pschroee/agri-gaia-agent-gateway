// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"fmt"
	"math"
	"strings"
)

// preprocessDiffs compares the preprocessing a call names with the one of outputs/model.json of the run,
// field by field (#184). Only the fields the call names are compared; a field the call leaves out takes the
// file's. A field the file does not name has the default the check container feeds (rgb, 1/255, nchw, no
// mean, no std; the size comes from the model, so a size is not compared without one in the file). Every
// difference is one part "<field> <call> (call) vs <run> (model.json)"; no part means equal.
func preprocessDiffs(call, run *PreprocessInput) []string {
	if call == nil {
		return nil
	}
	if run == nil {
		run = &PreprocessInput{}
	}
	var out []string
	add := func(field, c, r string) {
		out = append(out, fmt.Sprintf("%s %s (call) vs %s (model.json)", field, c, r))
	}
	if call.Color != "" {
		r := run.Color
		if r == "" {
			r = "rgb"
		}
		if call.Color != r {
			add("color", call.Color, r)
		}
	}
	if len(call.Size) > 0 && len(run.Size) > 0 {
		cs, rs := squareSize(call.Size), squareSize(run.Size)
		if cs[0] != rs[0] || cs[1] != rs[1] {
			add("size", fmt.Sprintf("%dx%d", cs[0], cs[1]), fmt.Sprintf("%dx%d", rs[0], rs[1]))
		}
	}
	if call.Scale != 0 {
		r := run.Scale
		suffix := ""
		if r == 0 {
			r, suffix = ScaleToUnit, " (default)"
		}
		if !floatClose(call.Scale, r) {
			add("scale", scaleText(call.Scale), scaleText(r)+suffix)
		}
	}
	if len(call.Mean) > 0 && !sameFloats(call.Mean, run.Mean) {
		add("mean", floatsText(call.Mean), floatsOrNone(run.Mean))
	}
	if len(call.Std) > 0 && !sameFloats(call.Std, run.Std) {
		add("std", floatsText(call.Std), floatsOrNone(run.Std))
	}
	if call.Layout != "" {
		r, suffix := run.Layout, ""
		if r == "" {
			r, suffix = "nchw", " (default)"
		}
		if call.Layout != r {
			add("layout", call.Layout, r+suffix)
		}
	}
	return out
}

// squareSize widens "size [h]" to [h, h]; the check container resolves a single value the same way.
func squareSize(s []int) [2]int {
	if len(s) == 1 {
		return [2]int{s[0], s[0]}
	}
	return [2]int{s[0], s[1]}
}

// sameFloats compares two per-channel lists; a single value stands for every channel (NumPy broadcast in
// the check container). An empty list (not named) is only equal to an empty list.
func sameFloats(a, b []float64) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	if (len(a) != 1 && len(a) != n) || (len(b) != 1 && len(b) != n) {
		return false
	}
	for i := 0; i < n; i++ {
		x, y := a[0], b[0]
		if len(a) > 1 {
			x = a[i]
		}
		if len(b) > 1 {
			y = b[i]
		}
		if !floatClose(x, y) {
			return false
		}
	}
	return true
}

// floatClose: equal within 1e-6 plus a relative 1e-3, so that 0.00392 is 1/255 but 0.13 is not 0.1307.
func floatClose(a, b float64) bool {
	return math.Abs(a-b) <= 1e-6+1e-3*math.Max(math.Abs(a), math.Abs(b))
}

func scaleText(v float64) string {
	if math.Abs(v-ScaleToUnit) < 1e-6 {
		return "1/255"
	}
	return fmt.Sprintf("%g", v)
}

func floatsText(v []float64) string {
	s := make([]string, len(v))
	for i, f := range v {
		s[i] = fmt.Sprintf("%g", f)
	}
	return strings.Join(s, "/")
}

func floatsOrNone(v []float64) string {
	if len(v) == 0 {
		return "none"
	}
	return floatsText(v)
}

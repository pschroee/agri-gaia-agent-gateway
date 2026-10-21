// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// The preflight of a final check (#181). A final check measures at the hold-out set, and the agent gets
// that test once. If the class order cannot be derived from the images the model saw (typically because
// the check feeds the images differently than the training did), the hold-out images must not go through
// the model at all: the check ends as not measured and the final test stays available. The preflight is
// one container run over the sample of seen images only.

// Collapse is the finding that a model answers (nearly) every seen image with the same output.
type Collapse struct {
	Output int
	K      int
	N      int
}

// collapseReason is the second reason of a report whose class order is unclear and whose model answers
// one output for nearly every image it was trained on. preSummary is set for an own model (the
// preprocessing the check used); empty for a model of a platform template.
func collapseReason(c Collapse, preSummary string) string {
	if preSummary == "" {
		return fmt.Sprintf("the model answers output %d for %d of %d images it was trained on: it has not learned to tell the classes apart",
			c.Output, c.K, c.N)
	}
	return fmt.Sprintf("the model answers output %d for %d of %d images it was trained on: the check probably feeds the images differently than the training did (preprocess: %s)",
		c.Output, c.K, c.N, preSummary)
}

// Short renders the preprocessing in one line for a reason, e.g. "gray 28x28, scale 1/255, mean 0.5, std 0.5".
func (p PreprocessFigures) Short() string {
	parts := []string{p.Color}
	if len(p.Size) == 2 {
		parts[0] += fmt.Sprintf(" %dx%d", p.Size[0], p.Size[1])
	}
	if math.Abs(p.Scale-ScaleToUnit) < 1e-6 {
		parts = append(parts, "scale 1/255")
	} else {
		parts = append(parts, fmt.Sprintf("scale %g", p.Scale))
	}
	if len(p.Mean) > 0 {
		parts = append(parts, "mean "+joinFloats(p.Mean))
	}
	if len(p.Std) > 0 {
		parts = append(parts, "std "+joinFloats(p.Std))
	}
	return strings.Join(parts, ", ")
}

func joinFloats(v []float64) string {
	s := make([]string, len(v))
	for i, f := range v {
		s[i] = fmt.Sprintf("%g", f)
	}
	return strings.Join(s, "/")
}

// sampleAnswers collects, for the images of the seen sample, the output the model answered and the class
// of the image; images the model could not run are left out.
func sampleAnswers(byID map[string]Prediction, sample []checkImage) (outIdx []int, truth []string) {
	for _, im := range sample {
		p, ok := byID[im.SHA]
		if !ok || !p.OK || len(p.Probs) == 0 {
			continue
		}
		outIdx = append(outIdx, p.Top)
		truth = append(truth, im.Class)
	}
	return outIdx, truth
}

// preflightResult is the answer of preflight: Stop true means the class order is not derivable and the
// final check ends without running the hold-out images.
type preflightResult struct {
	Stop bool
	// Reason: why the class order is unclear (DeriveMapping).
	Reason string
	// Collapse: the second reason when the model answers one output for nearly all seen images; empty
	// otherwise.
	Collapse string
}

// preflight runs the model on the sample of seen images only and asks whether the class order can be
// derived (NT1). A model that does not load, takes no single image input or is no classification does
// not stop here: the main run reports that in its own words, and nothing of the hold-out set is learned
// from it. preSummary is empty for a platform template and names the preprocessing for an own model.
func (s *Service) preflight(ctx context.Context, job Job, sample []checkImage, preSummary string) (preflightResult, error) {
	job.Images = imagesOf(sample)
	out, err := s.Run.Run(ctx, job)
	if err != nil {
		return preflightResult{}, err
	}
	if out.Error != "" || out.Model.LoadError != "" || len(out.Model.Inputs) != 1 {
		return preflightResult{}, nil
	}
	_, outputs, notClassification := classificationShape(out.Model)
	if notClassification {
		return preflightResult{}, nil
	}
	byID := map[string]Prediction{}
	for _, p := range out.Predictions {
		byID[p.ID] = p
	}
	idx, truth := sampleAnswers(byID, sample)
	if len(idx) == 0 {
		return preflightResult{}, nil
	}
	_, ok, reason := DeriveMapping(idx, truth, outputs)
	if ok {
		return preflightResult{}, nil
	}
	res := preflightResult{Stop: true, Reason: reason}
	if o, k, n, collapsed := collapsedOutput(idx, truth); collapsed {
		res.Collapse = collapseReason(Collapse{Output: o, K: k, N: n}, preSummary)
	}
	return res, nil
}

// preflightReasons lists the reasons of a stopped final check: the class order first, then the collapse.
func preflightReasons(r preflightResult) []string {
	reasons := []string{ClassOrderUnclear(r.Reason)}
	if r.Collapse != "" {
		reasons = append(reasons, r.Collapse)
	}
	return reasons
}

// suggestionCheckPreprocess replaces retrain for an own model whose answers collapse (NT4): a new
// training does not help when the check feeds the images differently than the script does.
var suggestionCheckPreprocess = Suggestion{ID: "check_preprocess",
	Text: "declare preprocess exactly as the training script feeds the images (color, size, scale, mean, std) or write outputs/model.json, then run the check again"}

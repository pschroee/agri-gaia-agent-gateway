// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// The checks of own models (R2) work on a model the agent wrote itself and had run in the notebook server.
// What such a model expects of an image is described as data, not as code (R2.4): the agent names the
// preprocessing and the class order it used, and the gateway checks the description before the call goes
// out. The check itself lives in internal/assess (own_model.go); this file holds what the tool validates.

// --- where an uploaded model came from (R2.6) ---

// OnModelUploaded is what the gateway does when a model was uploaded to the platform: if the file came out
// of a notebook run of the chat, the model keeps that origin, so that a later check of the model names the
// check of its file. The platform package knows no store, so main.go wires it (like OnModelCreated, R1).
type OnModelUploaded func(ctx context.Context, chatID string, modelID int64, sha256 string)

var onModelUploaded OnModelUploaded

// SetOnModelUploaded wires the hook. Not wired: an uploaded model gets no origin, and a check of it answers
// "origin unknown" (P2).
func SetOnModelUploaded(f OnModelUploaded) { onModelUploaded = f }

// NotifyModelUploaded reports a model an upload of this chat created, with the SHA-256 of the file the
// gateway read for that call (R2.6). The manager calls it with the answer of a successful POST /models: it
// is there that the uploads of the call and the id of the created model come together. The hook decides
// what the SHA means — a file that no notebook run handed over records nothing.
func NotifyModelUploaded(ctx context.Context, chatID string, res Result, uploads []Upload) {
	if onModelUploaded == nil || len(uploads) == 0 {
		return
	}
	if modelID := modelIDOf(res); modelID > 0 {
		onModelUploaded(ctx, chatID, modelID, uploads[0].SHA256)
	}
}

// --- what an own model declares about itself (R2.4) ---

// maxClassNames bounds the class names an own model may declare: the same limit as the outputs the check
// container accepts (assess.MaxClasses).
const maxClassNames = 1000

// checkClassNames checks the class order the agent declared for its own model (R2.4): one name per output,
// each a short one-line string. The order is only compared with the order the gateway derives; a wrong one
// is a reason of the report, never a refusal of the call.
func checkClassNames(names []string) error {
	if len(names) == 0 {
		return errors.New("class_names must name at least one class")
	}
	if len(names) > maxClassNames {
		return fmt.Errorf("class_names: at most %d classes", maxClassNames)
	}
	for i, n := range names {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("class_names[%d] is empty", i)
		}
		if len([]rune(n)) > 200 {
			return fmt.Errorf("class_names[%d] is longer than 200 characters", i)
		}
		if strings.ContainsAny(n, "\n\r\t") || strings.ContainsRune(n, 0) {
			return fmt.Errorf("class_names[%d] must be one line", i)
		}
	}
	return nil
}

// preprocessSpec is the preprocessing an own model was written for (R2.4), as the agent declares it: the
// same fields the container job carries (assess.Pre, R0.12). Values that are missing stay at the default
// of the platform templates (rgb, the model's image size, 1/255, no mean and no std, nchw).
type preprocessSpec struct {
	Color  string    `json:"color"`
	Size   []int     `json:"size"`
	Scale  float64   `json:"scale"`
	Mean   []float64 `json:"mean"`
	Std    []float64 `json:"std"`
	Layout string    `json:"layout"`
}

// checkPreprocess checks the declared preprocessing (R2.4), answers the object for the check and whether
// the declaration names anything at all (an empty object is no declaration: the template's default
// applies). An invalid value is a refusal of the call — the gateway would otherwise measure a model it
// does not know how to feed.
func checkPreprocess(raw json.RawMessage) ([]byte, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p preprocessSpec
	if err := dec.Decode(&p); err != nil {
		return nil, false, fmt.Errorf("preprocess: %v (fields: color, size, scale, mean, std, layout)", err)
	}
	names := p.Color != "" || p.Layout != "" || p.Scale != 0 || len(p.Size) > 0 || len(p.Mean) > 0 || len(p.Std) > 0
	// The script reads "rgb" for every colour that is not "gray" (predict.py), so anything else is a
	// mistake in the declaration and not a third option.
	switch p.Color {
	case "", "rgb", "gray":
	default:
		return nil, false, fmt.Errorf("preprocess.color must be rgb or gray, not %q", p.Color)
	}
	switch p.Layout {
	case "", "nchw", "nhwc":
	default:
		return nil, false, fmt.Errorf("preprocess.layout must be nchw or nhwc, not %q", p.Layout)
	}
	if len(p.Size) > 2 {
		return nil, false, errors.New("preprocess.size: give the height or [height, width]")
	}
	for _, d := range p.Size {
		if d <= 0 || d > 100000 {
			return nil, false, fmt.Errorf("preprocess.size: %d is no image size", d)
		}
	}
	if p.Scale < 0 || math.IsNaN(p.Scale) || math.IsInf(p.Scale, 0) {
		return nil, false, errors.New("preprocess.scale must be a number of at least 0 (default 1/255)")
	}
	for _, f := range []struct {
		name   string
		values []float64
	}{{"mean", p.Mean}, {"std", p.Std}} {
		name, values := f.name, f.values
		if len(values) != 0 && len(values) != 1 && len(values) != 3 {
			return nil, false, fmt.Errorf("preprocess.%s: give one value or one per channel (three)", name)
		}
		for _, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, false, fmt.Errorf("preprocess.%s holds a value that is not a number", name)
			}
			if name == "std" && v == 0 {
				return nil, false, errors.New("preprocess.std: a standard deviation of 0 would divide by zero")
			}
		}
	}
	checked, err := json.Marshal(p)
	return checked, names, err
}

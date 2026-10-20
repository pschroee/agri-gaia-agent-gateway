// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agw/internal/plan"
)

// Modes of an objective: a larger or a smaller value is better.
const (
	ModeMax = "max"
	ModeMin = "min"
)

// Metrics are the figures a search may choose by, with the direction that fits each: only validation figures,
// so that the agent cannot pick the winner by a figure the model was trained on (issue #168).
var Metrics = map[string]string{"val_acc": ModeMax, "val_loss": ModeMin, "val_f1": ModeMax}

// DefaultMetric is the objective of a search that names none.
const DefaultMetric = "val_acc"

// Objective is what a search optimises: one validation figure and its direction.
type Objective struct {
	Metric string `json:"metric"`
	Mode   string `json:"mode"`
}

// ParseObjective reads {"metric","mode"}; both are optional, the metric defaults to val_acc and the mode to the
// direction that fits the metric. A mode that contradicts the metric is an error.
func ParseObjective(raw []byte) (Objective, error) {
	o := Objective{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		canon, err := plan.Canonical(raw)
		if err != nil {
			return o, fmt.Errorf("objective: %w", err)
		}
		if err := strictDecode(canon, &o); err != nil {
			return o, fmt.Errorf("objective: %w", err)
		}
	}
	if o.Metric == "" {
		o.Metric = DefaultMetric
	}
	want, ok := Metrics[o.Metric]
	if !ok {
		return o, fmt.Errorf("objective.metric %q is not allowed; choose one of: %s", o.Metric, strings.Join(metricNames(), ", "))
	}
	if o.Mode == "" {
		o.Mode = want
	}
	if o.Mode != want {
		return o, fmt.Errorf("objective.mode for %s must be %s", o.Metric, want)
	}
	return o, nil
}

func metricNames() []string {
	out := make([]string, 0, len(Metrics))
	for k := range Metrics {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Better reports whether a is better than b for this objective (strictly).
func (o Objective) Better(a, b float64) bool {
	if o.Mode == ModeMin {
		return a < b
	}
	return a > b
}

// Reached reports whether a value has reached a target (at least as good).
func (o Objective) Reached(value, target float64) bool {
	if o.Mode == ModeMin {
		return value <= target
	}
	return value >= target
}

// strictDecode decodes JSON into v and refuses fields v does not know and data after the value.
func strictDecode(raw []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("data after the JSON value")
	}
	return nil
}

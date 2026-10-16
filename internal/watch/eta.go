// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import "time"

// MinETAPercent is the smallest percent an estimate is based on (D2.4): below it the remaining time is
// so far off that naming it would be a guess.
const MinETAPercent = 5.0

// Sample is one measurement of a job's progress.
type Sample struct {
	At      time.Time
	Percent float64
}

// ETA estimates when a job ends, from its start and its measurements (D2.4): linear,
// eta = last measurement + (last measurement − start) × (100 − p) / p. nil while fewer than two
// measurements exist, while the percent is unknown, below MinETAPercent or already 100 — an estimate
// only from what the platform reported, never from a guess about the job.
func ETA(started time.Time, samples []Sample) *time.Time {
	if len(samples) < 2 {
		return nil
	}
	last := samples[len(samples)-1]
	p := last.Percent
	if p < MinETAPercent || p >= 100 || started.IsZero() || last.At.Before(started) {
		return nil
	}
	elapsed := last.At.Sub(started)
	eta := last.At.Add(time.Duration(float64(elapsed) * (100 - p) / p))
	return &eta
}

// Percent turns a fraction the platform reported (0..1) into the percent of the job (0..100); nil while
// the platform reports nothing (Agri-Gaia's tasks_api keeps completion_percentage as a fraction, and its
// frontend multiplies by 100 as well).
func Percent(fraction float64) *float64 {
	if fraction <= 0 {
		return nil
	}
	p := fraction * 100
	if p > 100 {
		p = 100
	}
	return &p
}

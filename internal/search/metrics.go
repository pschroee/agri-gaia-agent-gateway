// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

// Limits of the parsers of what a run writes (issue #168).
const (
	MaxMetricsLines = 1000
	MaxMetricsLine  = 4 << 10
	MaxGPULines     = 2000
)

// ParseMetricsJSONL reads the lines of metrics.jsonl: one JSON object per epoch. The rules (line length, no
// duplicate keys, key shape, finite numbers, RFC 3339 time, anything else dropped without an error) are
// implemented by TS-W in this file; until then it returns nothing.
func ParseMetricsJSONL(raw []byte) []Epoch {
	_ = raw
	return nil
}

// ParseGPUCSV reads the lines of gpu.csv: timestamp, utilisation (0 to 100), memory used and memory total in MB.
// Implemented by TS-W in this file; until then it returns nothing.
func ParseGPUCSV(raw []byte) []GPUSample {
	_ = raw
	return nil
}

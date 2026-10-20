// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agw/internal/plan"
)

// Limits of the parsers of what a run writes (issue #168).
const (
	MaxMetricsLines = 1000
	MaxMetricsLine  = 4 << 10
	MaxGPULines     = 2000
	// MaxMetricsKeys is the number of keys one line of metrics.jsonl may carry.
	MaxMetricsKeys = 32
)

// metricKey is the shape of a key of metrics.jsonl.
var metricKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ParseMetricsJSONL reads the lines of metrics.jsonl: one JSON object per epoch. What a run writes is not trusted,
// so every line stands for itself and anything that does not fit is dropped without an error:
//   - a line longer than MaxMetricsLine bytes, one that is no JSON object or has a key twice (plan.Canonical reads
//     it token by token), more than MaxMetricsKeys keys, a key that is not [a-z][a-z0-9_]{0,31};
//   - "epoch" is a whole number >= 0 (required), "epochs" a whole number >= 1, "at" a time in RFC 3339, "saved" a
//     boolean; the other four may be missing, a wrong type drops the line;
//   - every other key counts as a figure when its value is a finite number; a value of another type is left out
//     (the line stays).
//
// Of more than MaxMetricsLines lines the last ones are read (the file only grows); of several lines with the same
// epoch the last wins, and the result is in the order of the epochs. A missing "at" gives the zero
// time; the caller decides what it stands for.
func ParseMetricsJSONL(raw []byte) []Epoch {
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) > MaxMetricsLines+1 {
		lines = lines[len(lines)-(MaxMetricsLines+1):]
	}
	var out []Epoch
	for _, l := range lines {
		l = bytes.TrimSpace(l)
		if len(l) == 0 || len(l) > MaxMetricsLine {
			continue
		}
		if e, ok := parseMetricsLine(l); ok {
			out = append(out, e)
		}
	}
	if len(out) > MaxMetricsLines {
		out = out[len(out)-MaxMetricsLines:]
	}
	return lastPerEpoch(out)
}

// lastPerEpoch keeps the last line of each epoch, in the order of the epochs: the line the template repeats at
// the end with "saved" replaces the epoch's first one, and extra or out-of-order lines (the learning-rate and
// batch-size tuners of Lightning) do not count twice.
func lastPerEpoch(in []Epoch) []Epoch {
	if len(in) < 2 {
		return in
	}
	idx := map[int]int{}
	var out []Epoch
	for _, e := range in {
		if i, ok := idx[e.Epoch]; ok {
			out[i] = e
			continue
		}
		idx[e.Epoch] = len(out)
		out = append(out, e)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Epoch < out[b].Epoch })
	return out
}

func parseMetricsLine(l []byte) (Epoch, bool) {
	if l[0] != '{' || !utf8.Valid(l) {
		return Epoch{}, false
	}
	canon, err := plan.Canonical(l) // rejects a duplicate key, trailing data and deep nesting
	if err != nil {
		return Epoch{}, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(canon, &obj); err != nil || len(obj) > MaxMetricsKeys {
		return Epoch{}, false
	}
	var e Epoch
	e.Values = map[string]float64{}
	haveEpoch := false
	for k, v := range obj {
		if !metricKey.MatchString(k) {
			return Epoch{}, false
		}
		switch k {
		case "epoch":
			n, ok := wholeNumber(v)
			if !ok || n < 0 {
				return Epoch{}, false
			}
			e.Epoch, haveEpoch = n, true
		case "epochs":
			n, ok := wholeNumber(v)
			if !ok || n < 1 {
				return Epoch{}, false
			}
			e.Epochs = n
		case "at":
			var s string
			if json.Unmarshal(v, &s) != nil {
				return Epoch{}, false
			}
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return Epoch{}, false
			}
			e.At = t
		case "saved":
			if json.Unmarshal(v, &e.Saved) != nil {
				return Epoch{}, false
			}
		default:
			if f, ok := finiteNumber(v); ok {
				e.Values[k] = f
			}
		}
	}
	if !haveEpoch {
		return Epoch{}, false
	}
	return e, true
}

// finiteNumber reads a JSON number; a string, a boolean, null, an object or a number that does not fit a float64
// is none.
func finiteNumber(v json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(v))
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// wholeNumber reads a JSON number with an integral value (3 and 3.0, not 3.5) that fits an int.
func wholeNumber(v json.RawMessage) (int, bool) {
	f, ok := finiteNumber(v)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<31 {
		return 0, false
	}
	return int(f), true
}

// gpuTimeLayouts are the forms of the time in a line of gpu.csv: RFC 3339 and what nvidia-smi writes
// ("2026/10/20 12:00:05.123", in the time zone of the container, UTC in the images of the platform).
var gpuTimeLayouts = []string{time.RFC3339Nano, "2006/01/02 15:04:05.000", "2006/01/02 15:04:05"}

// ParseGPUCSV reads the lines of gpu.csv: timestamp, utilisation (0 to 100), memory used and memory total in MB,
// as `nvidia-smi --query-gpu=timestamp,utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits`
// writes them. A line that has not exactly these four fields, a time that is none, a utilisation outside 0 to 100
// or a memory that is no whole number >= 0 ("[N/A]") is dropped. Of more than MaxGPULines lines the last ones are
// read.
func ParseGPUCSV(raw []byte) []GPUSample {
	lines := strings.Split(string(raw), "\n")
	if len(lines) > MaxGPULines+1 {
		lines = lines[len(lines)-(MaxGPULines+1):]
	}
	var out []GPUSample
	for _, l := range lines {
		f := strings.Split(strings.TrimSpace(l), ",")
		if len(f) != 4 {
			continue
		}
		if s, ok := gpuSample(f[0], f[1], f[2], f[3]); ok {
			out = append(out, s)
		}
	}
	return out
}

// GPUSampleOf builds a sample from the four fields as text, the checks of ParseGPUCSV included; the training
// route of the platform hands the same fields over as JSON.
func GPUSampleOf(at, util, used, total string) (GPUSample, bool) {
	return gpuSample(at, util, used, total)
}

func gpuSample(at, util, used, total string) (GPUSample, bool) {
	var s GPUSample
	at = strings.TrimSpace(at)
	ok := false
	for _, layout := range gpuTimeLayouts {
		if t, err := time.Parse(layout, at); err == nil {
			s.At, ok = t.UTC(), true
			break
		}
	}
	if !ok {
		return s, false
	}
	u, err := strconv.ParseFloat(strings.TrimSpace(util), 64)
	if err != nil || math.IsNaN(u) || u < 0 || u > 100 {
		return s, false
	}
	s.Util = u
	m1, err1 := strconv.Atoi(strings.TrimSpace(used))
	m2, err2 := strconv.Atoi(strings.TrimSpace(total))
	if err1 != nil || err2 != nil || m1 < 0 || m2 < 0 {
		return s, false
	}
	s.MemUsedMB, s.MemTotalMB = m1, m2
	return s, true
}

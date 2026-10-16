// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"regexp"
	"strconv"
	"strings"
)

// The figure the training template printed itself (R1.13). The gateway reads it out of the training log
// and names it as the platform's figure — it never becomes the basis of the verdict (the platform
// measures on a split of its own that the gateway cannot see). The agent no longer reads the log for the
// score; the platform's own score pattern does not match Lightning's table, which is where "None Acc."
// in a model name came from (1.1).

// The names the templates print, in the order the gateway prefers them.
var logFigureOrder = []string{"MulticlassAccuracy", "Accuracy", "BinaryAccuracy", "MulticlassF1Score", "F1Score", "Precision", "Recall"}

var (
	// lightningFigureRe matches one row of Lightning's metric table, with and without the box-drawing
	// characters of the table borders: an optional Multiclass/Binary prefix, the metric and the value.
	lightningFigureRe = regexp.MustCompile(`(?i)^\W*(Multiclass|Binary)?(Accuracy|F1Score|Precision|Recall)\W+([0-9]*\.?[0-9]+(e-?[0-9]+)?)\W*$`)
	// yoloLineRe matches the summary line of Ultralytics ("all 120 120 0.99 0.98 0.99 0.97"):
	// images, instances, precision, recall, mAP50, mAP50-95.
	yoloLineRe = regexp.MustCompile(`^\s+all\s+(\d+)\s+(\d+)\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)`)
)

// PlatformFigure is the figure of the training template as the gateway read it out of the log (R1.13).
// Source names where it comes from, Note what it is worth (the template measured on its own internal
// split). A log without a recognisable figure leaves ok false.
type PlatformFigure struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Source string  `json:"source"`
	Note   string  `json:"note"`
}

// TrainLog is what the gateway reads out of one training log.
type TrainLog struct {
	// Figure: the metric the template printed (the last value of its name; ok false: none was found).
	Figure PlatformFigure
	Have   bool
	// NaN: a line names a nan loss, so the training did not converge.
	NaN bool
	// YOLO: the log carries Ultralytics' summary line and the fields that belong to the figure.
	Images, Instances int
}

// ParseTrainLog reads the platform's own figure out of a training log (R1.13). Lightning writes a metric
// table (often several: the last value of a name wins); Ultralytics writes one line per split with the
// row for all classes.
func ParseTrainLog(log string) TrainLog {
	var out TrainLog
	values := map[string]float64{}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimRight(line, "\r")
		if isNaNLine(line) {
			out.NaN = true
		}
		if m := lightningFigureRe.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseFloat(m[3], 64); err == nil {
				values[metricName(m[1], m[2])] = v
			}
			continue
		}
		if m := yoloLineRe.FindStringSubmatch(line); m != nil {
			images, _ := strconv.Atoi(m[1])
			instances, _ := strconv.Atoi(m[2])
			value, err := strconv.ParseFloat(m[6], 64)
			if err == nil {
				out.Images, out.Instances = images, instances
				out.Figure, out.Have = PlatformFigure{Name: "mAP50-95", Value: value}, true
			}
		}
	}
	if out.Have {
		return out
	}
	for _, name := range logFigureOrder {
		if v, ok := values[name]; ok {
			out.Figure, out.Have = PlatformFigure{Name: name, Value: v}, true
			return out
		}
	}
	return out
}

// isNaNLine reports whether a line names a nan loss: the training diverged instead of learning.
func isNaNLine(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "loss") && strings.Contains(l, "nan")
}

// metricName is the name of a metric as Lightning writes it: the prefix in its usual spelling plus the
// metric ("MulticlassAccuracy").
func metricName(prefix, metric string) string {
	if prefix == "" {
		return metric
	}
	p := strings.ToUpper(prefix[:1]) + strings.ToLower(prefix[1:])
	return p + metric
}

// PlatformFigureNote is the note of the platform's figure (R1.13, H114): what it is and what it is not.
// It carries no "platform figure" prefix — the check card writes that label itself. images is how many
// images the template trained on; the template puts about a fifth of them aside for its own test.
func PlatformFigureNote(images int) string {
	return "measured by the template on its own internal test split of about " +
		strconv.Itoa(testSplitImages(images)) + " images; the gateway cannot see which images"
}

// testSplitShare is the share of its images the templates put aside for their own test (the default of
// `test-split`, templates/Torchvision/Classification/EfficientNet/config/config.jsonschema).
const testSplitShare = 0.2

// testSplitImages rounds the internal test split of a training dataset.
func testSplitImages(images int) int {
	if images <= 0 {
		return 0
	}
	return int(float64(images)*testSplitShare + 0.5)
}

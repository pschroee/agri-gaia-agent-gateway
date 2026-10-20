// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import "time"

// Epoch is one line of the figures a run wrote (metrics.jsonl): the epoch, the number of epochs, the time and
// the figures by name (train_loss, val_loss, train_acc, val_acc, val_f1, lr and whatever else the run reports).
// Saved marks the line written once more when the run saved its model.
type Epoch struct {
	Epoch  int
	Epochs int
	At     time.Time
	Values map[string]float64
	Saved  bool
}

// GPUSample is one line of the GPU sampling of a run (gpu.csv): time, utilisation in percent and memory in MB.
type GPUSample struct {
	At         time.Time
	Util       float64
	MemUsedMB  int
	MemTotalMB int
}

// Score is the value of a trial for the objective: the figure of the **last** epoch, or of the last line marked
// saved when there is one, because the value belongs to the model that is saved and can be adopted. It is not the
// best value over all epochs, which would describe a model that does not exist. ok is false when there is no line
// or the line has no value for the metric.
func Score(obj Objective, epochs []Epoch) (value float64, epoch int, ok bool) {
	pick := -1
	for i := range epochs {
		if epochs[i].Saved {
			pick = i
		}
	}
	if pick < 0 {
		pick = len(epochs) - 1
	}
	if pick < 0 {
		return 0, 0, false
	}
	v, found := epochs[pick].Values[obj.Metric]
	if !found {
		return 0, 0, false
	}
	return v, epochs[pick].Epoch, true
}

// Result is a finished or running trial as the choice of the winner sees it.
type Result struct {
	Idx   int
	Done  bool
	Score *float64
}

// Winner picks the best trial that finished (state done) and has a score; with equal scores the earlier trial
// wins. ok is false when there is none.
func Winner(obj Objective, results []Result) (idx int, score float64, ok bool) {
	for _, r := range results {
		if !r.Done || r.Score == nil {
			continue
		}
		if !ok || obj.Better(*r.Score, score) || (*r.Score == score && r.Idx < idx) {
			idx, score, ok = r.Idx, *r.Score, true
		}
	}
	return idx, score, ok
}

// SinceImprovement counts the finished trials after the best one (the patience of the budget counts these).
// results must be in the order the trials ran.
func SinceImprovement(obj Objective, results []Result) int {
	best, since, have := 0.0, 0, false
	for _, r := range results {
		if !r.Done || r.Score == nil {
			continue
		}
		if !have || obj.Better(*r.Score, best) {
			best, since, have = *r.Score, 0, true
			continue
		}
		since++
	}
	return since
}

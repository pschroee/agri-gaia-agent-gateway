// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

// Progress and remaining time of a plan (issue #280). The functions are pure: they take the plan's order and the latest
// attempt per run key and call no API.

import (
	"math"
	"sort"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

// ETA bases.
const (
	etaBasisPlan = "plan"
	etaBasisTask = "task"
)

// expProgressView is the progress of a plan. The counts cover the whole plan, whatever --cell selects, so that the
// figure does not depend on the order of the cells.
type expProgressView struct {
	PlanEnded int     `json:"plan_ended"`
	PlanTotal int     `json:"plan_total"`
	Percent   float64 `json:"percent"`
	CellEnded int     `json:"cell_ended"`
	CellTotal int     `json:"cell_total"`
	// MeanRunS is the mean duration of the ended runs in seconds, null while none has ended.
	MeanRunS *float64 `json:"mean_run_s"`
	// ETAS is the estimated remaining time of the open runs in seconds, null without an estimate. Pauses between runs
	// (model state, backup) are not part of it.
	ETAS *int64 `json:"eta_s"`
	// ETABasis says what the estimate rests on: "task" when every open run used the mean of its own task, "plan" when
	// at least one used the mean of all ended runs.
	ETABasis string `json:"eta_basis,omitempty"`
}

// expCounts counts ended runs by outcome. Limits are the three limit_* statuses; Pass counts runs judged online as passed.
type expCounts struct {
	Done    int `json:"done"`
	Error   int `json:"error"`
	Aborted int `json:"aborted"`
	Limits  int `json:"limits"`
	Pass    int `json:"pass"`
}

func (c *expCounts) add(run agwclient.ExpRun) {
	switch run.Status {
	case "done":
		c.Done++
	case "error":
		c.Error++
	case "aborted":
		c.Aborted++
	case "limit_cost", "limit_tokens", "limit_duration":
		c.Limits++
	}
	if run.Pass != nil && *run.Pass {
		c.Pass++
	}
}

// expRunEnded reports whether the latest attempt of a run is over.
func expRunEnded(latest map[string]agwclient.ExpRun, key string) (agwclient.ExpRun, bool) {
	run, ok := latest[key]
	return run, ok && run.Status != expStarted
}

// expRunDuration is the wall-clock time of an attempt from the protocol's started_at and ended_at.
func expRunDuration(run agwclient.ExpRun) (time.Duration, bool) {
	start, err1 := time.Parse(time.RFC3339Nano, run.StartedAt)
	end, err2 := time.Parse(time.RFC3339Nano, run.EndedAt)
	if err1 != nil || err2 != nil || end.Before(start) {
		return 0, false
	}
	return end.Sub(start), true
}

// expProgress counts the runs of order whose latest attempt has ended. cell selects the cell_* counts; empty leaves them 0.
func expProgress(order []expPlannedRun, latest map[string]agwclient.ExpRun, cell string) expProgressView {
	v := expProgressView{PlanTotal: len(order)}
	var open []expPlannedRun
	var ended []agwclient.ExpRun
	for _, pr := range order {
		run, ok := expRunEnded(latest, pr.Key)
		if cell != "" && pr.Cell == cell {
			v.CellTotal++
		}
		if !ok {
			open = append(open, pr)
			continue
		}
		v.PlanEnded++
		ended = append(ended, run)
		if cell != "" && pr.Cell == cell {
			v.CellEnded++
		}
	}
	if v.PlanTotal > 0 {
		v.Percent = math.Round(float64(v.PlanEnded)/float64(v.PlanTotal)*1000) / 10
	}
	if mean, ok := expMeanDuration(ended); ok {
		s := math.Round(mean.Seconds()*10) / 10
		v.MeanRunS = &s
	}
	if len(open) == 0 {
		zero := int64(0)
		v.ETAS = &zero
	} else if eta, basis, ok := expETA(open, ended); ok {
		s := int64(math.Round(eta.Seconds()))
		v.ETAS, v.ETABasis = &s, basis
	}
	return v
}

func expMeanDuration(runs []agwclient.ExpRun) (time.Duration, bool) {
	var sum time.Duration
	n := 0
	for _, run := range runs {
		if d, ok := expRunDuration(run); ok {
			sum += d
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / time.Duration(n), true
}

// expETA estimates the time the open runs still need: for each the mean duration of the ended runs of the same task,
// else the mean of all ended runs. Without an ended run with a known duration there is no estimate (ok false).
func expETA(open []expPlannedRun, ended []agwclient.ExpRun) (eta time.Duration, basis string, ok bool) {
	byTask := map[string][]agwclient.ExpRun{}
	for _, run := range ended {
		byTask[run.Task] = append(byTask[run.Task], run)
	}
	planMean, havePlan := expMeanDuration(ended)
	if !havePlan {
		return 0, "", false
	}
	taskMean := map[string]time.Duration{}
	for task, runs := range byTask {
		if m, ok := expMeanDuration(runs); ok {
			taskMean[task] = m
		}
	}
	basis = etaBasisTask
	for _, pr := range open {
		if m, ok := taskMean[pr.Task]; ok {
			eta += m
			continue
		}
		eta += planMean
		basis = etaBasisPlan
	}
	return eta, basis, true
}

// expCellCount is the per-cell figure of agw exp status --progress.
type expCellCount struct {
	Planned int `json:"planned"`
	Ended   int `json:"ended"`
	Started int `json:"started"`
	expCounts
}

// expPlanCounts counts the ended runs of order over the whole plan.
func expPlanCounts(order []expPlannedRun, latest map[string]agwclient.ExpRun) expCounts {
	var c expCounts
	for _, pr := range order {
		if run, ok := expRunEnded(latest, pr.Key); ok {
			c.add(run)
		}
	}
	return c
}

// expCellCounts counts the ended runs of one cell.
func expCellCounts(order []expPlannedRun, latest map[string]agwclient.ExpRun, cell string) expCounts {
	var sub []expPlannedRun
	for _, pr := range order {
		if pr.Cell == cell {
			sub = append(sub, pr)
		}
	}
	return expPlanCounts(sub, latest)
}

// expLastError is one entry of last_errors.
type expLastError struct {
	Key     string `json:"key"`
	Attempt string `json:"attempt"`
	Time    string `json:"time"`
	Reason  string `json:"reason"`
}

// expStatusReport is the output of agw exp status --progress.
type expStatusReport struct {
	Plan       string                  `json:"plan"`
	Progress   expProgressView         `json:"progress"`
	Cells      map[string]expCellCount `json:"cells"`
	LastErrors []expLastError          `json:"last_errors"`
}

// expProgressReport builds the report from the whole order of the plan and the runs of its protocol.
func expProgressReport(plan string, order []expPlannedRun, runs []agwclient.ExpRun) expStatusReport {
	latest := expLatestByKey(runs)
	rep := expStatusReport{Plan: plan, Progress: expProgress(order, latest, ""), Cells: map[string]expCellCount{}, LastErrors: []expLastError{}}
	for _, pr := range order {
		c := rep.Cells[pr.Cell]
		c.Planned++
		if run, ok := latest[pr.Key]; ok {
			if run.Status == expStarted {
				c.Started++
			} else {
				c.Ended++
				c.add(run)
			}
		}
		rep.Cells[pr.Cell] = c
	}
	var errs []agwclient.ExpRun
	for _, pr := range order {
		if run, ok := latest[pr.Key]; ok && run.Status == "error" {
			errs = append(errs, run)
		}
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].EndedAt > errs[j].EndedAt })
	for i, run := range errs {
		if i == 5 {
			break
		}
		rep.LastErrors = append(rep.LastErrors, expLastError{Key: run.Key, Attempt: run.ID, Time: run.EndedAt, Reason: truncate(run.EndReason, 300)})
	}
	return rep
}

// expFullOrder is the order of every cell of the plan (what the progress counts), whatever --cell selects.
func expFullOrder(name string, p *exp.Plan, tasks []exp.Task, k int, seed int64) []expPlannedRun {
	cells := make([]string, 0, len(p.Cells))
	for _, c := range p.Cells {
		cells = append(cells, c.ID)
	}
	return expRunOrder(name, tasks, cells, k, seed, cellRunsTask(p))
}

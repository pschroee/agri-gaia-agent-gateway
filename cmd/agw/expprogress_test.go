// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

// endedRun is an ended attempt of key that took secs seconds.
func endedRun(pr expPlannedRun, status string, secs int) agwclient.ExpRun {
	start := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	return agwclient.ExpRun{ID: pr.Key, Key: pr.Key, Cell: pr.Cell, Task: pr.Task, Rep: pr.Rep, Status: status,
		StartedAt: start.Format(time.RFC3339Nano), EndedAt: start.Add(time.Duration(secs) * time.Second).Format(time.RFC3339Nano)}
}

func progressOrder() []expPlannedRun {
	return expRunOrder("p", []exp.Task{{ID: "a"}, {ID: "b"}}, []string{"c1", "c2"}, 2, 7, nil)
}

// #280 E8: the share counts the whole plan, whatever --cell selects; the cell figures count only the named cell.
func TestProgressPlanWideWithCell(t *testing.T) {
	order := progressOrder() // 8 runs, 4 per cell
	latest := map[string]agwclient.ExpRun{}
	cellEnded := 0
	ended := 0
	for _, pr := range order {
		if ended == 3 {
			break
		}
		latest[pr.Key] = endedRun(pr, "done", 60)
		ended++
		if pr.Cell == "c1" {
			cellEnded++
		}
	}
	// A run that stayed "started" is not ended.
	open := order[ended]
	latest[open.Key] = agwclient.ExpRun{ID: open.Key, Key: open.Key, Status: "started"}

	v := expProgress(order, latest, "c1")
	if v.PlanTotal != 8 || v.PlanEnded != 3 || v.Percent != 37.5 {
		t.Fatalf("plan figures: %+v", v)
	}
	if v.CellTotal != 4 || v.CellEnded != cellEnded {
		t.Fatalf("cell figures: %+v (want %d of 4 ended)", v, cellEnded)
	}
	if none := expProgress(order, latest, ""); none.CellTotal != 0 || none.CellEnded != 0 || none.PlanEnded != 3 {
		t.Fatalf("without a cell: %+v", none)
	}
	if v.MeanRunS == nil || *v.MeanRunS != 60 || v.ETAS == nil || *v.ETAS != 5*60 {
		t.Fatalf("mean and estimate: %+v", v)
	}
}

// #280 E8: an open run is estimated with the mean of the ended runs of its own task, else with the mean of all.
func TestETATaskMeanThenPlanMean(t *testing.T) {
	ended := []agwclient.ExpRun{
		{Task: "a", Status: "done", StartedAt: "2026-11-28T00:00:00Z", EndedAt: "2026-11-28T00:01:00Z"}, // 60 s
		{Task: "a", Status: "done", StartedAt: "2026-11-28T00:00:00Z", EndedAt: "2026-11-28T00:02:00Z"}, // 120 s
		{Task: "c", Status: "done", StartedAt: "2026-11-28T00:00:00Z", EndedAt: "2026-11-28T00:05:00Z"}, // 300 s
	}
	eta, basis, ok := expETA([]expPlannedRun{{Task: "a"}, {Task: "a"}}, ended)
	if !ok || eta != 180*time.Second || basis != "task" {
		t.Fatalf("task means: %s %s %v", eta, basis, ok)
	}
	// Task b has no ended run: it takes the plan mean of 160 s; task a takes its own 90 s.
	eta, basis, ok = expETA([]expPlannedRun{{Task: "a"}, {Task: "b"}}, ended)
	if !ok || eta != 250*time.Second || basis != "plan" {
		t.Fatalf("plan mean fallback: %s %s %v", eta, basis, ok)
	}
}

// #280 E8: without an ended run with a duration there is no estimate: eta_s and mean_run_s are null in the JSON.
func TestETANullWithoutEndedRuns(t *testing.T) {
	if _, _, ok := expETA([]expPlannedRun{{Task: "a"}}, nil); ok {
		t.Fatal("an estimate without data")
	}
	// An ended run without times does not count either.
	order := progressOrder()
	latest := map[string]agwclient.ExpRun{order[0].Key: {Key: order[0].Key, Status: "done"}}
	v := expProgress(order, latest, "")
	if v.PlanEnded != 1 || v.ETAS != nil || v.MeanRunS != nil {
		t.Fatalf("progress: %+v", v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"eta_s":null`) || !strings.Contains(string(raw), `"mean_run_s":null`) {
		t.Fatalf("json: %s", raw)
	}
}

// #280 E13: agw exp status --progress prints the progress of the whole plan, the counts per cell and the newest errors.
func TestStatusProgressJSON(t *testing.T) {
	dir := writeRunnerMaterial(t)
	m, err := loadMaterial(dir)
	if err != nil {
		t.Fatal(err)
	}
	order := expFullOrder(m.PlanID, m.Plan, m.Tasks, m.Plan.K, m.Plan.Seed)
	var runs []agwclient.ExpRun
	for i, pr := range order[:5] {
		status := "done"
		switch i {
		case 1:
			status = "error"
		case 2:
			status = "limit_tokens"
		case 4:
			status = "started"
		}
		run := endedRun(pr, status, 60)
		run.EndReason = fmt.Sprintf("reason %d", i)
		run.EndedAt = time.Date(2026, 11, 28, 1, i, 0, 0, time.UTC).Format(time.RFC3339)
		if status == "started" {
			run.EndedAt = ""
		}
		runs = append(runs, run)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/exp/runs" || r.URL.Query().Get("plan") != m.PlanID {
			t.Errorf("unexpected request %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(runs)
	}))
	defer srv.Close()
	t.Setenv("AGW_EXP_API_TOKEN", "exp-key")
	code, out, errw := runAgw(t, "exp", "status", "--plan", dir, "--progress", "--url", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var got struct {
		Plan     string `json:"plan"`
		Progress struct {
			PlanEnded int      `json:"plan_ended"`
			PlanTotal int      `json:"plan_total"`
			ETAS      *float64 `json:"eta_s"`
		} `json:"progress"`
		Cells map[string]struct {
			Planned, Ended, Started, Done, Error, Limits int
		} `json:"cells"`
		LastErrors []struct {
			Key, Time, Reason string
		} `json:"last_errors"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Plan != m.PlanID || got.Progress.PlanTotal != 8 || got.Progress.PlanEnded != 4 || got.Progress.ETAS == nil {
		t.Fatalf("report: %+v", got)
	}
	planned, ended := 0, 0
	for _, c := range got.Cells {
		planned += c.Planned
		ended += c.Ended
	}
	if len(got.Cells) != 2 || planned != 8 || ended != 4 {
		t.Fatalf("cells: %+v", got.Cells)
	}
	if len(got.LastErrors) != 1 || got.LastErrors[0].Key != order[1].Key || got.LastErrors[0].Reason != "reason 1" || got.LastErrors[0].Time == "" {
		t.Fatalf("last errors: %+v", got.LastErrors)
	}
}

// #280 E13: last_errors lists the five newest runs that ended in error, newest first.
func TestProgressReportLastErrorsNewestFiveFirst(t *testing.T) {
	order := progressOrder()
	var runs []agwclient.ExpRun
	for i, pr := range order[:7] {
		run := endedRun(pr, "error", 10)
		run.EndedAt = time.Date(2026, 11, 28, 2, i, 0, 0, time.UTC).Format(time.RFC3339)
		run.EndReason = fmt.Sprint(i)
		runs = append(runs, run)
	}
	rep := expProgressReport("p", order, runs)
	if len(rep.LastErrors) != 5 || rep.LastErrors[0].Reason != "6" || rep.LastErrors[4].Reason != "2" {
		t.Fatalf("last errors: %+v", rep.LastErrors)
	}
}

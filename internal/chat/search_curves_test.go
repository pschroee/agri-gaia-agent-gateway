// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

type curvesAnswer struct {
	Runs []struct {
		Label  string           `json:"label"`
		Trial  int              `json:"trial"`
		Params json.RawMessage  `json:"params"`
		Epochs []map[string]any `json:"epochs"`
	} `json:"runs"`
}

func curvesCall(t *testing.T, e *env, chatID string, q map[string]string) (platform.Result, curvesAnswer) {
	t.Helper()
	res, err := e.m.SearchDoer().Do(context.Background(), chatID, platform.Request{Method: http.MethodGet, Path: platform.CurvesPath, Query: q})
	if err != nil {
		t.Fatal(err)
	}
	var out curvesAnswer
	if res.Status == "ok" {
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			t.Fatalf("answer: %s (%v)", res.Body, err)
		}
	}
	return res, out
}

func addEpochs(t *testing.T, e *env, trialID string, n int) {
	t.Helper()
	var lines []store.TrialEpoch
	for i := 1; i <= n; i++ {
		lines = append(lines, store.TrialEpoch{Epoch: i, At: time.Now(),
			Data: json.RawMessage(`{"train_loss":0.5,"val_loss":0.6,"train_acc":0.8,"val_acc":0.7}`)})
	}
	if err := e.st.AddTrialEpochs(context.Background(), trialID, lines); err != nil {
		t.Fatal(err)
	}
}

// training_curves answers the epochs of a training of this chat and of no other chat.
func TestTrainingCurvesOwnChatOnly(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	tr, err := r.e.st.DeclareTrial(ctx, store.Trial{ChatID: r.a.ID, Path: store.SearchPathTemplate, Ref: "145", Params: json.RawMessage(`{"lr":0.01}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.st.StartTrial(ctx, tr.ID, "145", ""); err != nil {
		t.Fatal(err)
	}
	addEpochs(t, r.e, tr.ID, 3)

	res, out := curvesCall(t, r.e, r.a.ID, map[string]string{"train_container_id": "145"})
	if res.Status != "ok" || len(out.Runs) != 1 || out.Runs[0].Label != "container 145" || len(out.Runs[0].Epochs) != 3 {
		t.Fatalf("own chat: %+v %+v", res, out)
	}
	if e := out.Runs[0].Epochs[2]; e["epoch"] != float64(3) || e["val_acc"] != 0.7 || e["train_loss"] != 0.5 {
		t.Errorf("epoch line: %v", e)
	}
	// Another chat does not see the container, and the answer says nothing else.
	if res, _ := curvesCall(t, r.e, r.b.ID, map[string]string{"train_container_id": "145"}); res.Status != "error" {
		t.Errorf("another chat: %+v", res)
	}
	// Exactly one of the two arguments, and well-formed ones.
	for _, q := range []map[string]string{{}, {"train_container_id": "145", "search_id": "s0123456789ab"}, {"train_container_id": "x"}, {"search_id": "../x"}} {
		if res, _ := curvesCall(t, r.e, r.a.ID, q); res.Status != "error" {
			t.Errorf("%v: %+v", q, res)
		}
	}
}

// For a search every run of its trials comes back with its parameters; declared trials that never ran do not.
func TestTrainingCurvesSearch(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	s := r.search(t, r.a.ID, store.SearchRunning, `{"max_runs":5}`)
	for i := 1; i <= 2; i++ {
		tr, err := r.e.st.DeclareTrial(ctx, store.Trial{ChatID: r.a.ID, SearchID: s.ID, Path: store.SearchPathTemplate, Params: json.RawMessage(`{"lr":0.1}`)})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.e.st.StartTrial(ctx, tr.ID, "c"+string(rune('0'+i)), "")
		addEpochs(t, r.e, tr.ID, 2*i)
		_, _ = r.e.st.FinishTrial(ctx, tr.ID, store.TrialDone, "")
	}
	if _, err := r.e.st.DeclareTrial(ctx, store.Trial{ChatID: r.a.ID, SearchID: s.ID, Path: store.SearchPathTemplate, Params: json.RawMessage(`{"lr":0.2}`)}); err != nil {
		t.Fatal(err)
	}
	res, out := curvesCall(t, r.e, r.a.ID, map[string]string{"search_id": s.ID})
	if res.Status != "ok" || len(out.Runs) != 2 {
		t.Fatalf("search: %+v %+v", res, out)
	}
	if out.Runs[0].Label != "trial 1" || len(out.Runs[0].Epochs) != 2 || len(out.Runs[1].Epochs) != 4 || string(out.Runs[1].Params) != `{"lr":0.1}` {
		t.Errorf("runs: %+v", out.Runs)
	}
	if res, _ := curvesCall(t, r.e, r.b.ID, map[string]string{"search_id": s.ID}); res.Status != "error" {
		t.Errorf("another chat: %+v", res)
	}
}

func TestCurvePointsThinned(t *testing.T) {
	var lines []store.TrialEpoch
	for i := 1; i <= 1000; i++ {
		lines = append(lines, store.TrialEpoch{Epoch: i, Data: json.RawMessage(`{"val_acc":0.5}`)})
	}
	got := curvePoints(lines)
	if len(got) != maxCurvePoints || got[0]["epoch"] != 1 || got[len(got)-1]["epoch"] != 1000 {
		t.Errorf("thinned: %d first %v last %v", len(got), got[0], got[len(got)-1])
	}
}

// The note at the end of a search names the curves and their data.
func TestRunEndNoteNamesCurves(t *testing.T) {
	m := &Manager{}
	got := m.endText(store.Search{ID: "s0123456789ab", Path: store.SearchPathTemplate}, search.Objective{Metric: "val_acc", Mode: "max"}, "budget_runs",
		&store.Trial{Idx: 1, Ref: "63"}, SearchHoldout{FinalState: "none"})
	for _, want := range []string{"Show the learning curves", "{{tool:training_curves}}", "search_id=s0123456789ab"} {
		if !strings.Contains(got, want) {
			t.Errorf("search end text lacks %q: %s", want, got)
		}
	}
}

// The note of a finished single training names the curves of its container.
func TestTrainingEndNoteNamesCurves(t *testing.T) {
	e, c, _, j := earlySetup(t, "", 0)
	ctx := context.Background()
	j.Kind, j.Ref = "training_run", "145"
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, Path: store.SearchPathTemplate, Ref: "145"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.StartTrial(ctx, tr.ID, "145", j.ID); err != nil {
		t.Fatal(err)
	}
	if line := e.m.trainingEndLines(ctx, j, watch.StateDone); !strings.Contains(line, "train_container_id=145") || !strings.Contains(line, "{{tool:training_curves}}") {
		t.Errorf("training note: %q", line)
	}
	if line := e.m.trainingEndLines(ctx, j, watch.StateFailed); line != "" {
		t.Errorf("a failed run needs no curves: %q", line)
	}
	other := j
	other.Kind = "platform_task"
	if line := e.m.trainingEndLines(ctx, other, watch.StateDone); line != "" {
		t.Errorf("another kind of job: %q", line)
	}
}

// With AGW_PLATFORM_GPU=expected a run without GPU samples gets the warning; unknown or present GPU gets none.
func TestCPURunWarningWhenGPUExpected(t *testing.T) {
	no, yes := false, true
	for _, c := range []struct {
		opt  string
		gpu  *bool
		want bool
	}{{"expected", &no, true}, {"expected", &yes, false}, {"expected", nil, false}, {"", &no, false}, {"none", &no, false}} {
		m := &Manager{opt: Options{PlatformGPU: c.opt}}
		got := m.cpuRunLine(store.Trial{GPUAvailable: c.gpu})
		if (got != "") != c.want {
			t.Errorf("%q %v: %q", c.opt, c.gpu, got)
		}
		if c.want && !strings.Contains(got, "used the CPU although the platform has a GPU") {
			t.Errorf("wording: %q", got)
		}
	}
}

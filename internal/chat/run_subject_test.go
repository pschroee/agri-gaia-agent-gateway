// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"testing"

	"agw/internal/store"
	"agw/internal/watch"
)

const realConfigShape = `{"config":{"train":{"values":{"lr":0.01,"epochs":4},"schema":{}}},"provider":"torch",` +
	`"architecture":{"name":"efficientnet_v2_s","category":"classification"},"dataset_id":177}`

// Issue #227: the platform answers the configuration of a container as {"config":{"train":…},"architecture":…}; the
// reader of the book of trials takes that shape (it read {"train":…} and found nothing).
func TestReadTrainConfigRealShape(t *testing.T) {
	e := setup(t)
	f := newFakeJobs(t, e, "/tasks/5")
	f.set("GET /train/containers/145/config", realConfigShape)
	c, err := e.m.Create(context.Background(), NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	doc, msg := readTrainConfig(context.Background(), e.m.summaryReader(c.ID), "145")
	if msg != "" {
		t.Fatal(msg)
	}
	if len(doc.Config.Train.Values) != 2 || doc.Architecture.Name != "efficientnet_v2_s" || doc.DatasetID != 177 || doc.Provider != "torch" {
		t.Fatalf("%+v", doc)
	}
}

func TestSingleTrialSubject(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/5")
	c, _ := e.m.Create(ctx, NewChat{})
	f.set("GET /train/containers/145/config", realConfigShape)
	f.set("GET /datasets/177", `{"name":"Pig tail biting","filecount":120,"annotation_labels":["a","b"]}`)
	j := store.WatchJob{ID: "w1", ChatID: c.ID, Kind: trialKindTraining, Ref: "145"}
	params, subject := e.m.singleRunInfo(ctx, j, store.SearchPathTemplate, watch.Progress{})
	var b SearchBase
	if err := json.Unmarshal(subject, &b); err != nil {
		t.Fatalf("subject %s: %v", subject, err)
	}
	if b.ContainerID != 145 || b.Architecture != "efficientnet_v2_s" || b.Dataset == nil || b.Dataset.Name != "Pig tail biting" {
		t.Fatalf("subject: %+v", b)
	}
	var p map[string]float64
	if json.Unmarshal(params, &p) != nil || p["epochs"] != 4 {
		t.Errorf("params: %s", params)
	}

	// The dataset cannot be read: the architecture stays, the dataset is named by its number.
	f.mu.Lock()
	f.status["GET /datasets/177"] = 500
	f.mu.Unlock()
	_, subject = e.m.singleRunInfo(ctx, j, store.SearchPathTemplate, watch.Progress{})
	b = SearchBase{}
	_ = json.Unmarshal(subject, &b)
	if subjectText(b) != "efficientnet_v2_s on dataset 177" {
		t.Errorf("title without the dataset name: %q", subjectText(b))
	}

	// The configuration cannot be read: no subject, the title falls back to the container.
	f.mu.Lock()
	f.status["GET /train/containers/145/config"] = 500
	f.mu.Unlock()
	params, subject = e.m.singleRunInfo(ctx, j, store.SearchPathTemplate, watch.Progress{})
	if subject != nil || string(params) != "{}" {
		t.Errorf("unreadable: params %s subject %s", params, subject)
	}
}

func TestSubjectText(t *testing.T) {
	ds := func(id int64, name string) *SearchNamed { return &SearchNamed{ID: id, Name: name} }
	for _, c := range []struct {
		b    SearchBase
		want string
	}{
		{SearchBase{ContainerID: 145, Architecture: "resnet18", Dataset: ds(1, "Pigs")}, "resnet18 on Pigs"},
		{SearchBase{ContainerID: 145, Architecture: "resnet18"}, "resnet18"},
		{SearchBase{ContainerID: 145, Dataset: ds(1, "Pigs")}, "Training on Pigs"},
		{SearchBase{ContainerID: 145, Architecture: "resnet18", Dataset: ds(177, "")}, "resnet18 on dataset 177"},
		{SearchBase{ContainerID: 145}, "Training container 145"},
		{SearchBase{}, "Training run"},
	} {
		if got := subjectText(c.b); got != c.want {
			t.Errorf("%+v: %q, want %q", c.b, got, c.want)
		}
	}
}

func TestSingleTrialTitle(t *testing.T) {
	sub, _ := json.Marshal(SearchBase{ContainerID: 145, Architecture: "resnet18", Dataset: &SearchNamed{ID: 1, Name: "Pigs"}})
	for _, c := range []struct {
		t     store.Trial
		entry string
		want  string
	}{
		{store.Trial{Path: store.SearchPathTemplate, Ref: "145", Subject: sub}, "", "resnet18 on Pigs"},
		{store.Trial{Path: store.SearchPathTemplate, Ref: "66"}, "", "Training container 66"},
		{store.Trial{Path: store.SearchPathTemplate}, "", "Training run"},
		{store.Trial{Path: store.SearchPathNotebook, Ref: "r1"}, "train.py", "train.py"},
		{store.Trial{Path: store.SearchPathNotebook, Ref: "r1"}, "", "Notebook run"},
	} {
		if got := singleTrialTitle(c.t, c.entry); got != c.want {
			t.Errorf("%+v: %q, want %q", c.t, got, c.want)
		}
	}
}

// A trial view carries the plan and the step of its job, so that the detail opened by its address knows them.
func TestTrialViewPlanStep(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	jobID, err := r.e.st.AddWatchJob(ctx, store.WatchJob{ChatID: r.a.ID, Kind: trialKindTraining, Ref: "9", PlanID: "plan-1", StepIdx: 3})
	if err != nil {
		t.Fatal(err)
	}
	tr := r.single(t, r.a.ID)
	if ok, err := r.e.st.StartTrial(ctx, tr.ID, "9", jobID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	tr, _ = r.e.st.GetTrial(ctx, r.a.ID, tr.ID)
	v := r.e.m.trialView(ctx, tr, nil)
	if v.PlanID != "plan-1" || v.PlanStep != 3 {
		t.Fatalf("plan %q step %d", v.PlanID, v.PlanStep)
	}
}

func earlySetup(t *testing.T, planID string, step int) (*env, store.Chat, *fakeJobs, store.WatchJob) {
	t.Helper()
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/5")
	c, err := e.st.CreateChat(ctx, store.NewChat{Title: "c", Model: "p/m", Variant: "cli", Owner: "anna"})
	if err != nil {
		t.Fatal(err)
	}
	f.set("GET /train/containers/145/config", realConfigShape)
	f.set("GET /datasets/177", `{"name":"Pig tail biting","filecount":120,"annotation_labels":["a"]}`)
	j := store.WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:5|train:145", PlanID: planID, StepIdx: step}
	j.ID, err = e.st.AddWatchJob(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	return e, c, f, j
}

// The call that starts a training lists it at once, with its plan step, before any poll.
func TestDeclareTrainingEarlyPlanStep(t *testing.T) {
	e, c, _, j := earlySetup(t, "plan-1", 3)
	ctx := context.Background()
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	tr, err := e.st.TrialByJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("no trial for the job: %v", err)
	}
	if tr.State != store.TrialRunning || tr.Ref != "145" || tr.Path != store.SearchPathTemplate || len(tr.Subject) == 0 {
		t.Fatalf("trial: %+v", tr)
	}
	if isTrialJob(j.ID) {
		t.Error("the job keeps the note for a job outside a plan: it is not marked as a trial's")
	}
	owner := "anna"
	page, _ := e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner, Kinds: []string{RunKindTraining}})
	if len(page.Runs) != 1 || page.Runs[0].PlanStep != 3 || page.Runs[0].PlanID != "plan-1" || page.Runs[0].Title != "efficientnet_v2_s on Pig tail biting" {
		t.Fatalf("%+v", page.Runs)
	}
	// A second call for the same job makes no second trial.
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	if list, _ := e.st.ListTrials(ctx, c.ID); len(list) != 1 {
		t.Errorf("trials: %d", len(list))
	}
}

func TestDeclareTrainingEarlyOutsidePlan(t *testing.T) {
	e, c, _, j := earlySetup(t, "", 0)
	ctx := context.Background()
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	if _, err := e.st.TrialByJob(ctx, j.ID); err != nil {
		t.Fatalf("a call outside a plan is listed too: %v", err)
	}
	// A job that starts no training (a dataset task) makes none.
	other, _ := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:6|config:12"})
	e.m.declareTrainingEarly(ctx, c.ID, other)
	if list, _ := e.st.ListTrials(ctx, c.ID); len(list) != 1 {
		t.Errorf("trials: %d", len(list))
	}
}

// The trial a search declared for the container takes its job itself: no second one is made.
func TestDeclareTrainingEarlySkipsSearchTrial(t *testing.T) {
	e, c, _, j := earlySetup(t, "", 0)
	ctx := context.Background()
	if _, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, Path: store.SearchPathTemplate, Ref: "145"}); err != nil {
		t.Fatal(err)
	}
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	if _, err := e.st.TrialByJob(ctx, j.ID); err == nil {
		t.Error("the job must not get a trial of its own")
	}
	if list, _ := e.st.ListTrials(ctx, c.ID); len(list) != 1 {
		t.Errorf("trials: %d", len(list))
	}
}

// When the task ends and the job becomes the training run, its first poll finds the early trial.
func TestHandoverKeepsEarlyTrial(t *testing.T) {
	e, c, f, j := earlySetup(t, "plan-1", 3)
	ctx := context.Background()
	f.set("GET /train/containers/145/metrics", trialMetricsBody(2, false))
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	first, _ := e.st.TrialByJob(ctx, j.ID)
	j.Kind, j.Ref = trialKindTraining, "145"
	recordTrialProgress(ctx, e.m, j, watch.Progress{State: watch.StateRunning})
	list, _ := e.st.ListTrials(ctx, c.ID)
	if len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("one trial for the job: %+v", list)
	}
}

// A task that fails before the container runs ends the early trial too.
func TestTaskFailureEndsEarlyTrial(t *testing.T) {
	e, c, _, j := earlySetup(t, "", 0)
	ctx := context.Background()
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	finishSingleTrial(ctx, e.m, j, watch.Progress{State: watch.StateFailed, Message: "image pull failed"})
	tr, _ := e.st.TrialByJob(ctx, j.ID)
	if tr.State != store.TrialFailed || tr.Error != "image pull failed" {
		t.Fatalf("trial: %+v", tr)
	}
}

// The reason of a failed training is the last error line of its log, else a plain sentence (not "exited").
func TestFailedTrainingErrorFromLog(t *testing.T) {
	e, c, _, j := earlySetup(t, "", 0)
	ctx := context.Background()
	e.m.declareTrainingEarly(ctx, c.ID, j.ID)
	j.Kind, j.Ref = trialKindTraining, "145"
	finishSingleTrial(ctx, e.m, j, watch.Progress{State: watch.StateFailed, Message: "exited",
		LogTail: "Epoch 1\nTraceback (most recent call last):\nRuntimeError: CUDA out of memory\nprocess ended\n"})
	tr, _ := e.st.TrialByJob(ctx, j.ID)
	if tr.State != store.TrialFailed || tr.Error != "RuntimeError: CUDA out of memory" {
		t.Fatalf("trial: %+v", tr)
	}

	e2, c2, _, j2 := earlySetup(t, "", 0)
	e2.m.declareTrainingEarly(ctx, c2.ID, j2.ID)
	j2.Kind, j2.Ref = trialKindTraining, "145"
	finishSingleTrial(ctx, e2.m, j2, watch.Progress{State: watch.StateFailed, Message: "exited", LogTail: "Epoch 1\n"})
	tr, _ = e2.st.TrialByJob(ctx, j2.ID)
	if tr.Error != trainingNoScoreText {
		t.Errorf("error: %q", tr.Error)
	}
}

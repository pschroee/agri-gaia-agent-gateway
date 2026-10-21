// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func floatPtr(f float64) *float64 { return &f }

// A-E2 (S1): a step that waits for an observed job. The step is claimed, goes to "waiting" and names the
// job; the progress of the job is read with the step (the plan API carries it); the step ends done, and a
// step that is already done cannot be made to wait again.
func TestWaitingStepLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Minute)
	if ok, err := s.DecidePlan(ctx, p.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	jobID, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, PlanID: p.ID, StepIdx: 1, Kind: "platform_task", Ref: "task:5"})
	if err != nil || jobID == "" {
		t.Fatalf("job: %q %v", jobID, err)
	}
	if ok, err := s.WaitPlanStep(ctx, p.ID, 1, jobID, "ok 202", "train_containers", "7"); err != nil || !ok {
		t.Fatalf("wait: ok=%v err=%v", ok, err)
	}
	got, err := s.GetPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st := got.Steps[0]; st.State != "waiting" || st.WatchJob != jobID {
		t.Fatalf("waiting step: %+v", st)
	}
	if st := got.Steps[0]; st.CreatedResource != "train_containers" || st.CreatedID != "7" {
		t.Fatalf("the object the call created is missing: %+v", st)
	}
	// A second wait on the same step does nothing: it is not open any more.
	if ok, err := s.WaitPlanStep(ctx, p.ID, 1, jobID, "", "", ""); err != nil || ok {
		t.Fatalf("second wait: ok=%v err=%v", ok, err)
	}

	// What the loop writes lands on the step as well.
	if err := s.SetJobProgress(ctx, jobID, WatchProgress{State: JobRunning, Percent: floatPtr(40),
		Message: "building", LogTail: "a\nb",
		Outputs: []WatchOutput{{Resource: "container_image", ID: "u/i:1", Size: 42}}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := got.Steps[0]
	if st.Progress == nil || *st.Progress != 40 || st.ProgressMsg != "building" || st.LogTail != "a\nb" {
		t.Fatalf("progress of the step: %+v", st)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].ID != "u/i:1" || st.Outputs[0].Size != 42 {
		t.Fatalf("outputs of the step: %+v", st.Outputs)
	}
	if st.JobStarted == nil {
		t.Fatalf("start of the job missing: %+v", st)
	}

	// The job ends, the step follows.
	if err := s.SetJobProgress(ctx, jobID, WatchProgress{State: JobDone}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishPlanStep(ctx, p.ID, 1, "waiting", "done", "job w1 done", "train_containers", "7"); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	got, err = s.GetPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].State != "done" {
		t.Fatalf("step after the job: %+v", got.Steps[0])
	}
	// The second step is the next one now.
	if next := got.Steps[1]; next.State != "open" {
		t.Fatalf("second step: %+v", next)
	}
	// The failed reading calls of a job are counted and cleared by a successful one.
	if err := s.SetJobProgress(ctx, jobID, WatchProgress{ReadFail: true}); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetWatchJob(ctx, c.ID, jobID)
	if err != nil || job.FailReads != 0 {
		t.Fatalf("a finished job counts no failure: %+v %v", job, err)
	}
}

// A-E2: the loop sees the open jobs of all chats; a chat's list shows its own jobs only, and an ended job
// is not open any more.
func TestWatchJobsAcrossChats(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c1, c2 := newPlanChat(t, s), newPlanChat(t, s)
	j1, err := s.AddWatchJob(ctx, WatchJob{ChatID: c1.ID, Kind: "platform_task", Ref: "task:1"})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := s.AddWatchJob(ctx, WatchJob{ChatID: c2.ID, Kind: "training_run", Ref: "9"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenWatchJobs(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("open jobs: %+v %v", open, err)
	}
	own, err := s.ChatWatchJobs(ctx, c1.ID)
	if err != nil || len(own) != 1 || own[0].ID != j1 {
		t.Fatalf("jobs of one chat: %+v %v", own, err)
	}
	if _, err := s.GetWatchJob(ctx, c2.ID, j1); err != ErrNotFound {
		t.Fatalf("a foreign job is not readable: %v", err)
	}
	if ok, err := s.FinishWatchJob(ctx, j1, JobDone); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	if ok, err := s.FinishWatchJob(ctx, j1, JobDone); err != nil || ok {
		t.Fatalf("finishing twice: ok=%v err=%v", ok, err)
	}
	open, err = s.OpenWatchJobs(ctx)
	if err != nil || len(open) != 1 || open[0].ID != j2 {
		t.Fatalf("open jobs after the end: %+v %v", open, err)
	}
}

// A-E2.2: a job outside a plan is a row of its own (plan_id empty, step 0) and is observed like any
// other; a step of a plan is not involved.
func TestWatchJobOutsidePlan(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	id, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "image_build", Ref: "task:3|image:u/i:1"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.GetWatchJob(ctx, c.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.PlanID != "" || job.StepIdx != 0 || job.State != JobRunning || job.Kind != "image_build" {
		t.Fatalf("job outside a plan: %+v", job)
	}
	if job.Ref != "task:3|image:u/i:1" {
		t.Fatalf("reference: %q", job.Ref)
	}
	open, err := s.OpenWatchJobs(ctx)
	if err != nil || len(open) != 1 || open[0].ID != id {
		t.Fatalf("open jobs: %+v %v", open, err)
	}
	// The chain of a started training: the row is observed on as another kind and cleans its slate.
	if err := s.SetJobProgress(ctx, id, WatchProgress{State: JobRunning, Percent: floatPtr(50), Message: "task"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReArmWatchJob(ctx, id, "training_run", "9"); err != nil || !ok {
		t.Fatalf("re-arm: ok=%v err=%v", ok, err)
	}
	job, err = s.GetWatchJob(ctx, c.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "training_run" || job.Ref != "9" || job.Progress != nil || job.Message != "" || job.CheckedAt != nil {
		t.Fatalf("after the hand-over: %+v", job)
	}
}

// The link of a job (own code C-E4) survives the round trip through the store and reaches every reading
// path; a job without one leaves the field out of its JSON, and the hand-over to another kind clears it.
func TestWatchJobLink(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	id, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "notebook_run", Ref: "r20260101120000-abcd"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh job carries none: the link appears with the first poll of its kind.
	job, err := s.GetWatchJob(ctx, c.ID, id)
	if err != nil || job.Link != "" {
		t.Fatalf("new job: %+v %v", job, err)
	}
	const link = "http://hub.example/user/agent-test/agw-1a2b3c4d/lab/tree/work/runs/r20260101120000-abcd"
	if err := s.SetJobProgress(ctx, id, WatchProgress{State: JobRunning, Message: "running", Link: link}); err != nil {
		t.Fatal(err)
	}
	if job, err = s.GetWatchJob(ctx, c.ID, id); err != nil || job.Link != link {
		t.Fatalf("link after the poll: %+v %v", job, err)
	}
	jobs, err := s.ChatWatchJobs(ctx, c.ID)
	if err != nil || len(jobs) != 1 || jobs[0].Link != link {
		t.Fatalf("link in the list of the chat: %+v %v", jobs, err)
	}
	open, err := s.OpenWatchJobs(ctx)
	if err != nil || len(open) != 1 || open[0].Link != link {
		t.Fatalf("link in the open jobs: %+v %v", open, err)
	}
	// The JSON of a job without a link carries no such field: the card builds no address of its own.
	plain, err := json.Marshal(WatchJob{ID: "w1", ChatID: c.ID, Kind: "notebook_run", State: JobRunning})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "link") {
		t.Errorf("a job without a link carries the field: %s", plain)
	}
	// The hand-over of the row to another kind cleans its slate, the link included.
	if ok, err := s.ReArmWatchJob(ctx, id, "training_run", "9"); err != nil || !ok {
		t.Fatalf("re-arm: ok=%v err=%v", ok, err)
	}
	if job, err = s.GetWatchJob(ctx, c.ID, id); err != nil || job.Link != "" {
		t.Fatalf("link after the hand-over: %+v %v", job, err)
	}
}

// Who stopped a job (own code C-E4, the words of issue #120/L3): the field is written once — a progress
// update and a second stop leave it alone —, an empty stopper is an error, and the hand-over of the row to
// another kind clears it with the rest of the slate.
func TestJobStoppedBy(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	id, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "notebook_run", Ref: "r20260101120000-abcd"})
	if err != nil {
		t.Fatal(err)
	}
	if job, err := s.GetWatchJob(ctx, c.ID, id); err != nil || job.StoppedBy != "" {
		t.Fatalf("a fresh job: %+v %v", job, err)
	}
	if _, err := s.SetJobStoppedBy(ctx, c.ID, "notebook_run", ""); err == nil {
		t.Error("an empty stopper must be an error")
	}
	ok, err := s.SetJobStoppedBy(ctx, c.ID, "notebook_run", StoppedByUser)
	if err != nil || !ok {
		t.Fatalf("set: ok=%v err=%v", ok, err)
	}
	// A stop of another kind of that chat marks nothing.
	if ok, err := s.SetJobStoppedBy(ctx, c.ID, "image_build", StoppedByAgent); err != nil || ok {
		t.Errorf("another kind: ok=%v err=%v", ok, err)
	}
	// The first stopper stands: a progress update and a second stop do not touch it.
	if err := s.SetJobProgress(ctx, id, WatchProgress{State: JobRunning, Message: "running", Link: "http://hub/x"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetJobStoppedBy(ctx, c.ID, "notebook_run", StoppedByAgent); err != nil || ok {
		t.Errorf("a second stopper: ok=%v err=%v", ok, err)
	}
	job, err := s.GetWatchJob(ctx, c.ID, id)
	if err != nil || job.StoppedBy != StoppedByUser || job.Link != "http://hub/x" {
		t.Fatalf("after progress and a second stop: %+v %v", job, err)
	}
	// The hand-over to another kind cleans the slate, the stopper included.
	if ok, err := s.ReArmWatchJob(ctx, id, "training_run", "9"); err != nil || !ok {
		t.Fatalf("re-arm: ok=%v err=%v", ok, err)
	}
	job, err = s.GetWatchJob(ctx, c.ID, id)
	if err != nil || job.StoppedBy != "" {
		t.Fatalf("after the hand-over: %+v %v", job, err)
	}
	// The JSON of a job without a stopper carries no such field.
	plain, err := json.Marshal(WatchJob{ID: "w1", ChatID: c.ID, Kind: "notebook_run", State: JobRunning})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "stopped_by") {
		t.Errorf("a job nobody stopped carries the field: %s", plain)
	}
	// A job that ended cannot be marked any more.
	if err := s.SetJobProgress(ctx, id, WatchProgress{State: JobDone}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetJobStoppedBy(ctx, c.ID, "training_run", StoppedByAgent); err != nil || ok {
		t.Errorf("a job that ended: ok=%v err=%v", ok, err)
	}
	if job, err = s.GetWatchJob(ctx, c.ID, id); err != nil || job.StoppedBy != "" {
		t.Fatalf("the stopper of a job that ended: %+v %v", job, err)
	}
}

// A-E2 (D2.8): a parked approval is marked on the step it belongs to, is found by its approval id, and
// only an open or running step can be parked.
func TestParkApprovalOnlyOpenStep(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Minute)
	if ok, err := s.DecidePlan(ctx, p.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ParkApproval(ctx, p.ID, 2, "ap-1"); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}
	st, err := s.PlanStepByParkedApproval(ctx, c.ID, "ap-1")
	if err != nil || st.Idx != 2 || st.PlanID != p.ID {
		t.Fatalf("parked step: %+v %v", st, err)
	}
	if _, err := s.PlanStepByParkedApproval(ctx, c.ID, "ap-2"); err != ErrNotFound {
		t.Fatalf("unknown approval: %v", err)
	}
	if ok, err := s.FinishPlanStep(ctx, p.ID, 2, "open", "done", "ok", "", ""); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ParkApproval(ctx, p.ID, 2, "ap-3"); err != nil || ok {
		t.Fatalf("parking a finished step: ok=%v err=%v", ok, err)
	}
}

// #180: of two polls that end a job only the first sees that it changed the row.
func TestUpdateJobProgressReportsClose(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	jobID, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:5"})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := s.UpdateJobProgress(ctx, jobID, WatchProgress{State: JobRunning, Message: "x"}); err != nil || !changed {
		t.Fatalf("running on an open row: changed=%v err=%v", changed, err)
	}
	if changed, err := s.UpdateJobProgress(ctx, jobID, WatchProgress{State: JobDone}); err != nil || !changed {
		t.Fatalf("first close: changed=%v err=%v", changed, err)
	}
	if changed, err := s.UpdateJobProgress(ctx, jobID, WatchProgress{State: JobDone}); err != nil || changed {
		t.Fatalf("second close must report no change: changed=%v err=%v", changed, err)
	}
}

// Issue #203: the run's own start and end are kept once set; a later poll without them leaves them alone.
func TestJobRunTimes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	jobID, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "notebook_run", Ref: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(36 * time.Second)
	if _, err := s.UpdateJobProgress(ctx, jobID, WatchProgress{State: JobRunning, RunStartedAt: &start}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateJobProgress(ctx, jobID, WatchProgress{State: JobDone, RunStartedAt: &start, RunEndedAt: &end}); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetWatchJob(ctx, c.ID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.RunStartedAt == nil || !j.RunStartedAt.Equal(start) || j.RunEndedAt == nil || !j.RunEndedAt.Equal(end) {
		t.Fatalf("run times: %v %v", j.RunStartedAt, j.RunEndedAt)
	}
	// An update without them (the server is gone) clears nothing; the row is closed, so write on a second row.
	id2, _ := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "notebook_run", Ref: "r2"})
	s.UpdateJobProgress(ctx, id2, WatchProgress{State: JobRunning, RunStartedAt: &start})
	s.UpdateJobProgress(ctx, id2, WatchProgress{State: JobRunning})
	j2, _ := s.GetWatchJob(ctx, c.ID, id2)
	if j2.RunStartedAt == nil || !j2.RunStartedAt.Equal(start) {
		t.Fatalf("run start lost: %v", j2.RunStartedAt)
	}
}

func TestJobRunTimesNilForOtherKinds(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	jobID, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateJobProgress(ctx, jobID, WatchProgress{State: JobDone}); err != nil {
		t.Fatal(err)
	}
	j, _ := s.GetWatchJob(ctx, c.ID, jobID)
	if j.RunStartedAt != nil || j.RunEndedAt != nil {
		t.Fatalf("run times on a kind without them: %v %v", j.RunStartedAt, j.RunEndedAt)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// uploadStepPlan stores an approved plan whose only step uploads n files as a dataset, with the sizes and hashes the
// socket pins, and returns the live call that is exactly that step, files read.
func uploadStepPlan(t *testing.T, e *env, ctx context.Context, chatID string, n int) platform.Request {
	t.Helper()
	var paths []string
	var sums []string
	var sizes []int
	for i := 0; i < n; i++ {
		paths = append(paths, fmt.Sprintf("/workspace/pu/%c/img_%04d.png", 'a'+i%5, i))
		data := fmt.Sprintf("image %d", i)
		sums = append(sums, upSum(data))
		sizes = append(sizes, len(data))
	}
	args, _ := json.Marshal(map[string]any{"name": "pu172", "description": "x", "files": paths})
	sh, _ := json.Marshal(sums)
	sz, _ := json.Marshal(sizes)
	approvedPlan(t, e, ctx, chatID, `{"title":"Upload","steps":[{"tool":"upload_dataset","args":`+string(args)+
		`,"file_sha256":`+string(sh)+`,"file_bytes":`+string(sz)+`}]}`)
	tool, ok := platform.Lookup("upload_dataset")
	if !ok {
		t.Fatal("no tool upload_dataset")
	}
	req, err := tool.Build(args)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range req.Files {
		data := fmt.Sprintf("image %d", i)
		req.Uploads = append(req.Uploads, platform.Upload{Field: f.Field, Name: fmt.Sprintf("img_%04d.png", i), Data: []byte(data), SHA256: upSum(data)})
	}
	return req
}

// A plan with a 500-file upload step runs after the plan's approval alone; the platform receives all 500 files.
func TestPlatformCallPlanUploadCovered(t *testing.T) {
	e := setup(t)
	got := capturePlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	req := uploadStepPlan(t, e, ctx, c.ID, 500)
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req); err != nil || r.Status != "ok" {
		t.Fatalf("covered upload step: %+v %v", r, err)
	}
	bodies := got()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "image 0") || !strings.Contains(bodies[0], "image 499") {
		t.Fatalf("the platform did not get all files: %d requests", len(bodies))
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 0 {
		t.Fatalf("approvals %v, want none beyond the plan", kinds)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.State != "completed" {
		t.Fatalf("plan state %s, want completed", p.State)
	}
}

// A file changed after the plan was approved: the call is refused with files_changed, nothing is sent and the step
// stays open (no fall-back to a single approval).
func TestPlatformCallPlanFilesChanged(t *testing.T) {
	e := setup(t)
	got := capturePlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	req := uploadStepPlan(t, e, ctx, c.ID, 20)
	changed := "changed content"
	req.Uploads[7].Data = []byte(changed)
	req.Uploads[7].SHA256 = upSum(changed)
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil || r.Status != "error" || r.Reason != "files_changed" || !strings.Contains(r.Message, "plan step 1") {
		t.Fatalf("changed file: %+v %v", r, err)
	}
	if len(got()) != 0 {
		t.Fatal("a request reached the platform")
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 0 {
		t.Fatalf("approvals %v: the refusal must not ask the user", kinds)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.Steps[0].State != plan.StepOpen {
		t.Fatalf("step after a refusal: %+v", p.Steps[0])
	}
}

// The step list is fetched apart from the plan: the plan event carries the summary only.
func TestPlanStepUploadEntries(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	uploadStepPlan(t, e, ctx, c.ID, 12)
	p := onlyPlan(t, e, ctx, c.ID)
	entries, err := e.m.PlanStepUploadEntries(ctx, c.ID, p.ID, 1)
	if err != nil || len(entries) != 12 {
		t.Fatalf("entries: %d %v", len(entries), err)
	}
	if _, err := e.m.PlanStepUploadEntries(ctx, c.ID, p.ID, 2); err == nil {
		t.Fatal("a step that does not exist has no entries")
	}
	out := redactPlanText(p)
	if out.Steps[0].Uploads == nil || len(out.Steps[0].Files) != 0 {
		t.Fatalf("the plan event must carry uploads, not files: %+v", out.Steps[0])
	}
	if strings.Contains(string(out.Steps[0].Uploads), "img_0003") {
		t.Fatal("the plan event carries the list of every file")
	}
}

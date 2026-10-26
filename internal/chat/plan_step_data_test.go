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
	"agw/internal/store"
)

// Issue #221: the platform call of a plan step is logged with the plan and the step, a call outside a plan with neither.
func TestLogPlatformCallWritesPlanStep(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	req := liveCall(t, "GET", "/train/containers/7/model", "")
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil || res.Status != "ok" || res.PlanStep == nil || res.PlanID == "" {
		t.Fatalf("step 1: %+v %v", res, err)
	}
	e.m.LogPlatformCall(ctx, slot, c.ID, "cli", "GET /train/containers/7/model", "ok 200", res)
	outside, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/datasets", ""))
	if err != nil {
		t.Fatal(err)
	}
	e.m.LogPlatformCall(ctx, slot, c.ID, "cli", "GET /datasets", "ok 200", outside)

	p := onlyPlan(t, e, ctx, c.ID)
	calls, err := e.st.ListSocketCalls(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var in, out *store.SocketCall
	for i := range calls {
		switch calls[i].Detail {
		case "GET /train/containers/7/model":
			in = &calls[i]
		case "GET /datasets":
			out = &calls[i]
		}
	}
	if in == nil || in.PlanID != p.ID || in.PlanStep == nil || *in.PlanStep != 1 {
		t.Fatalf("call of step 1: %+v", in)
	}
	if out == nil || out.PlanID != "" || out.PlanStep != nil {
		t.Fatalf("call outside the plan: %+v", out)
	}
}

// The platform's own words end up in the result of a failed step (one line, tokens redacted, clipped).
func TestPlanFinishFailureLine(t *testing.T) {
	e := setup(t)
	withPlanPlatformPatch(t, e, http.StatusUnprocessableEntity)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	body := `{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`
	if r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/42", body)); r.Status == "ok" {
		t.Fatalf("step 2 should fail: %+v", r)
	}
	st := onlyPlan(t, e, ctx, c.ID).Steps[1]
	if st.State != "failed" || !strings.HasPrefix(st.Result, "error 422: failed") || strings.Contains(st.Result, "\n") {
		t.Fatalf("result of the failed step: %q", st.Result)
	}
}

func TestFailureLine(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, c := range []struct {
		name string
		res  platform.Result
		want func(string) bool
	}{
		{"message first", platform.Result{Message: "name already taken", Body: `{"detail":"other"}`}, func(s string) bool { return s == "name already taken" }},
		{"detail text", platform.Result{Body: `{"detail":"Dataset not found"}`}, func(s string) bool { return s == "Dataset not found" }},
		{"detail list", platform.Result{Body: `{"detail":[{"loc":["body","name"],"msg":"field required"},{"msg":"second"}]}`}, func(s string) bool { return s == "field required" }},
		{"one line", platform.Result{Message: "first\n  second\tthird"}, func(s string) bool { return s == "first second third" }},
		{"token redacted", platform.Result{Message: "bad token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlc2lnbmF0dXJl"}, func(s string) bool { return !strings.Contains(s, "eyJhbGci") }},
		{"clipped", platform.Result{Message: long}, func(s string) bool { return len([]rune(s)) <= 202 && strings.HasSuffix(s, "…") }},
		{"nothing usable", platform.Result{Body: `<html>`}, func(s string) bool { return s == "" }},
	} {
		if got := failureLine(c.res); !c.want(got) {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

// Issue #221: the last epoch of a poll is stored on the job and the plan step, and the job event carries it; a
// poll without figures keeps the old value.
func TestPollJobStoresLastEpoch(t *testing.T) {
	e, c, f, jobID := singleRunSetup(t)
	ctx := context.Background()
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	f.set("GET /train/containers/9/metrics", trialMetricsBody(2, false))
	runWatchersOnce(t, e)

	j, err := e.st.GetWatchJob(ctx, c.ID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var got store.EpochFigures
	if err := json.Unmarshal(j.LastEpoch, &got); err != nil || got.Epoch != 1 || got.Epochs != 4 || got.Values["val_acc"] != 0.2 {
		t.Fatalf("last_epoch of the job: %s %v", j.LastEpoch, err)
	}
	seen := false
	deadline := time.After(2 * time.Second)
	for !seen {
		select {
		case ev := <-events:
			if je, ok := ev.Data.(JobEvent); ok && ev.Kind == "job" && len(je.Job.LastEpoch) > 0 {
				seen = true
			}
		case <-deadline:
			t.Fatal("no job event with last_epoch")
		}
	}

	// No figures this time: the stored epoch stays.
	f.set("GET /train/containers/9/metrics", `{"supported":true,"epochs":[],"gpu":[]}`)
	runWatchersOnce(t, e)
	j, _ = e.st.GetWatchJob(ctx, c.ID, jobID)
	if err := json.Unmarshal(j.LastEpoch, &got); err != nil || got.Epoch != 1 {
		t.Fatalf("last_epoch after a poll without figures: %s %v", j.LastEpoch, err)
	}
}

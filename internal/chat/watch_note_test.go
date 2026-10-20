// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// A-E2 (D2.1): the answer of a call that started an observed job says so, so that the agent ends its turn
// instead of keeping it open and polling the platform itself (seen live on 16.10.2026). For an autonomous
// plan the gateway keeps the promise "you will be woken" (D2.6).
func TestWatchedStepAnswerTellsTheAgent(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, _ := watchedChat(t, e, ctx, watchPlanBody)
	req, err := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/train/config"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.HTTPStatus != http.StatusAccepted {
		t.Fatalf("the step did not run: %+v", res)
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepWaiting || steps[0].WatchJob == "" {
		t.Fatalf("the step does not wait for a job: %+v", steps[0])
	}
	if !strings.Contains(res.Note, steps[0].WatchJob) || !strings.Contains(res.Note, "step 1") {
		t.Fatalf("the answer does not name the observed job: %+v", res)
	}
	if !strings.Contains(res.Note, "end your turn") || !strings.Contains(res.Note, "wakes you") {
		t.Fatalf("the answer does not tell the agent that the gateway wakes it: %+v", res)
	}
	// The agent reads the note in every binding: the CLI and MCP render Text(), the REST binding the JSON.
	if text := res.Text(); !strings.Contains(text, "end your turn") || !strings.Contains(text, "HTTP 202") {
		t.Fatalf("the note does not reach the agent:\n%s", text)
	}
}

// A-E2 (D2.6): a plan that does not wake the agent gets no promise that it will be woken — nothing would
// keep it, and the agent follows the job itself like before A-E2.
func TestWatchedStepAnswerWithoutAPromise(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p, _ := watchedChat(t, e, ctx, `{"title":"Watch","steps":[{"method":"POST","path":"/train/config"}]}`)
	slot := e.m.live[c.ID].slot.ID
	req, err := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/train/config"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil {
		t.Fatal(err)
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepWaiting {
		t.Fatalf("the interactive plan's step does not wait for its job: %+v", steps[0])
	}
	if strings.Contains(res.Note, "wakes you") {
		t.Fatalf("an interactive plan must not promise a wake-up: %+v", res)
	}
	if !strings.Contains(res.Note, "keep an eye on it yourself") || !strings.Contains(res.Note, steps[0].WatchJob) {
		t.Fatalf("the answer does not tell the agent what to do instead: %+v", res)
	}
}

// A-E2.2: a job outside a plan is observed for the cards only — no note, no promise, no wake-up.
func TestJobOutsidePlanAnswerHasNoNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	newFakeJobs(t, e, "/tasks/7")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	res := callWithApproval(t, e, ctx, c.ID, slot, platform.Request{Method: http.MethodPost, Path: "/train/config"})
	if res.Status != "ok" {
		t.Fatalf("call: %+v", res)
	}
	if res.Note != "" {
		t.Fatalf("a job outside a plan must not promise anything: %+v", res)
	}
	jobs, err := e.m.Jobs(ctx, c.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("the job is not observed for the cards: %+v %v", jobs, err)
	}
}

// Issue #141: a token in a stored platform message or log tail never reaches the note to the agent, also
// when the row was written before the gateway redacted platform answers.
func TestJobNoteRedactsTokens(t *testing.T) {
	const token = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudC10ZXN0In0.c2lnbmF0dXJl"
	note := JobNote(store.WatchJob{ID: "w1", Kind: "task",
		Message: "assume role failed: " + token,
		LogTail: "WARN token " + token + " expired"}, nil, watch.StateFailed, time.Second, nil)
	text := note.Text()
	if strings.Contains(text, token) {
		t.Fatalf("the token reached the note: %s", text)
	}
	if got := strings.Count(text, platform.RedactText(token)); got != 2 {
		t.Fatalf("expected two marks in the note, got %d:\n%s", got, text)
	}
}

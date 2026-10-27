// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
)

// splitPlanOf is a plan of one split step on dataset 185; body is the step's body.
func splitPlanOf(body string) string {
	return `{"title":"Split","steps":[{"method":"POST","path":"` + platform.AssessSplitPath + `","body":` + body + `}]}`
}

// withDatasetPlatform answers GET /datasets/185 with the given status and body; everything else is {}.
func withDatasetPlatform(t *testing.T, e *env, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/datasets/185" {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
}

// splitMatch approves a plan with the split step, makes the call and returns what planNext answers.
func splitMatch(t *testing.T, stepBody, callBody string) (*store.PlanStep, string, platform.Request) {
	t.Helper()
	e := setup(t)
	withDatasetPlatform(t, e, http.StatusOK, `{"name":"demo-pruefung"}`)
	return splitMatchOn(t, e, stepBody, callBody)
}

func splitMatchOn(t *testing.T, e *env, stepBody, callBody string) (*store.PlanStep, string, platform.Request) {
	t.Helper()
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	approvedPlan(t, e, ctx, c.ID, splitPlanOf(stepBody))
	req := liveCall(t, http.MethodPost, platform.AssessSplitPath, callBody)
	st, _, differs, out := e.m.planNext(ctx, c.ID, req)
	return st, differs, out
}

func TestPlanSplitDefaultNameCovered(t *testing.T) {
	st, differs, out := splitMatch(t, `{"dataset_id":185,"train_share":70}`,
		`{"dataset_id":185,"name":"demo-pruefung-train","train_share":70}`)
	if st == nil {
		t.Fatalf("the call must be the step, differs in %q", differs)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, has := body["name"]; has {
		t.Fatalf("the step runs without the name: %s", out.Body)
	}
}

func TestPlanSplitOtherNameAsks(t *testing.T) {
	st, differs, out := splitMatch(t, `{"dataset_id":185,"train_share":70}`,
		`{"dataset_id":185,"name":"other","train_share":70}`)
	if st != nil || differs != "body" {
		t.Fatalf("a different name must not match: step=%v differs=%q", st, differs)
	}
	if !strings.Contains(string(out.Body), `"other"`) {
		t.Fatalf("the call must stay as sent: %s", out.Body)
	}
}

func TestPlanSplitDefaultReadFails(t *testing.T) {
	e := setup(t)
	withDatasetPlatform(t, e, http.StatusNotFound, `{"detail":"not found"}`)
	st, differs, _ := splitMatchOn(t, e, `{"dataset_id":185,"train_share":70}`,
		`{"dataset_id":185,"name":"demo-pruefung-train","train_share":70}`)
	if st != nil || differs != "body" {
		t.Fatalf("a failed read must fail closed: step=%v differs=%q", st, differs)
	}
}

func TestPlanSplitExplicitShareCovered(t *testing.T) {
	// The step comes from the tool (which writes the default share); the call names it explicitly.
	st, differs, _ := splitMatch(t, `{"dataset_id":185,"train_share":70}`, `{"dataset_id":185,"train_share":70}`)
	if st == nil {
		t.Fatalf("the call must be the step, differs in %q", differs)
	}
}

func TestDerivedDefaultsOnlyGatewayPaths(t *testing.T) {
	for path := range derivedDefaults {
		if !strings.HasPrefix(path, "/_agw/") {
			t.Errorf("derived default for %q: only paths the gateway answers itself may be entered", path)
		}
	}
}

// B19 (demo 5 of 27.10.2026): the final test in a plan leaves eval_dataset_id out; the agent names the source of the
// split explicitly, which is the dataset the check takes without it. That call is the step; any other id is not.
func TestPlanCheckDefaultEvalDatasetCovered(t *testing.T) {
	for _, tc := range []struct {
		call string
		want bool
	}{
		{`{"final":true,"model_id":51,"eval_dataset_id":212}`, true},
		{`{"final":true,"model_id":51}`, true},
		{`{"final":true,"model_id":51,"eval_dataset_id":213}`, false},
		{`{"final":true,"model_id":51,"eval_dataset_id":999}`, false},
	} {
		e := setup(t)
		withDatasetPlatform(t, e, http.StatusOK, `{"name":"demo"}`)
		ctx := store.WithSession(context.Background(), "main")
		c, err := e.m.Create(ctx, NewChat{})
		if err != nil {
			t.Fatal(err)
		}
		// dataset 212 was split into 213; model 51 was trained on 213
		if err := e.st.RecordSplit(ctx, store.Split{ChatID: c.ID, SourceDatasetID: 212, TrainDatasetID: 213, TrainImages: 210, KeptImages: 90}); err != nil {
			t.Fatal(err)
		}
		if err := e.st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 51, ChatID: c.ID, Source: "training", TrainContainerID: 156, SeenDatasetIDs: []int64{213}}); err != nil {
			t.Fatal(err)
		}
		approvedPlan(t, e, ctx, c.ID, `{"title":"Check","steps":[{"method":"POST","path":"`+platform.AssessCheckPath+`","body":{"final":true,"model_id":51}}]}`)
		st, _, differs, _ := e.m.planNext(ctx, c.ID, liveCall(t, http.MethodPost, platform.AssessCheckPath, tc.call))
		if got := st != nil; got != tc.want {
			t.Errorf("%s: covered=%v, want %v (differs in %q)", tc.call, got, tc.want, differs)
		}
	}
}

// B19: the note at the end of a search inside a plan points to the plan's next steps and suggests no extra argument.
func TestPlanSearchEndTextNoFreeHints(t *testing.T) {
	score := 0.9
	got := planSearchEndText(store.Search{ID: "s0123456789ab", Path: store.SearchPathTemplate},
		search.Objective{Metric: "val_acc", Mode: "max"}, "budget_runs", &store.Trial{Idx: 2, Ref: "156", Score: &score}, 4)
	for _, want := range []string{"step 4 of the approved plan", "{{step4.id}} is now training container 156", "exactly as approved", "do not add eval_dataset_id"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "eval_dataset_id=") || strings.Contains(got, "propose_plan") {
		t.Errorf("a free hint in the plan's end note: %q", got)
	}
}

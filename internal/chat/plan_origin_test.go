// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// withOriginPlatform answers what the proposal and the start of a deployment read: the installed templates
// (id 4 comes from the official repository, id 9 from another one), the device, the image and the
// deployment 9. It returns the calls that arrived.
func withOriginPlatform(t *testing.T, e *env) func() []string {
	t.Helper()
	device := edgeDeviceStateJSON("sim-edge-01", "amd64", []string{"simulated"}, edgeHeartbeatNow(), true)
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, edgeJWT("agent-test"))
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/inference-container-templates":
			fmt.Fprintf(w, `[{"id":4,"name":"official","git_url":%q},{"id":9,"name":"other","git_url":"https://example.invalid/t"}]`, platform.AllowedTemplateGitURL)
		case r.Method == http.MethodGet && r.URL.Path == "/edge-devices":
			fmt.Fprint(w, "["+device+"]")
		case r.Method == http.MethodGet && r.URL.Path == "/container-images":
			fmt.Fprint(w, `[{"id":87,"repository":"agent-test/img","tag":"v1","platform":"linux/amd64","exposed_ports":[]}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/container-deployments/9":
			fmt.Fprint(w, `{"id":9,"name":"agw-img","status":"created","edge_device_id":1,"container_image_id":87}`)
		case r.Method == http.MethodPut && r.URL.Path == "/container-deployments/9/deploy":
			w.Header().Set("Location", "/tasks/5")
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// buildDeployPlan builds an image from template tmpl, creates a deployment of it and starts it.
func buildDeployPlan(tmpl int) string {
	return fmt.Sprintf(`{"title":"Build and deploy","steps":[
 {"method":"POST","path":"/container-images/build","body":{"container_template_id":%d,"model_id":6,"repository":"agent-test/img","tag":"v1","architecture":"amd64"}},
 {"method":"POST","path":"/container-deployments","body":{"edge_device_id":1,"container_image_id":"{{step1.id}}","name":"agw-img","port_bindings":[{"host_port":"18119","container_port":"8000","protocol":"tcp"}]}},
 {"method":"PUT","path":"/container-deployments/{{step2.id}}/deploy"}]}`, tmpl)
}

// finishSteps marks the first steps of the plan done, as if their calls had run: the build created image 87,
// the creation deployment 9.
func finishSteps(t *testing.T, e *env, ctx context.Context, planID string, n int) {
	t.Helper()
	created := []struct{ resource, id string }{{"container_image", "87"}, {"container_deployment", "9"}}
	for i := 0; i < n; i++ {
		if ok, err := e.st.ClaimPlanStep(ctx, planID, i+1); err != nil || !ok {
			t.Fatalf("claim %d: %v %v", i+1, ok, err)
		}
		if ok, err := e.st.FinishPlanStep(ctx, planID, i+1, plan.StepRunning, plan.StepDone, "ok", created[i].resource, created[i].id); err != nil || !ok {
			t.Fatalf("finish %d: %v %v", i+1, ok, err)
		}
	}
}

// ownDeployment records deployment id as created in the chat, as the creating call would (edge guard D-E1).
func ownDeployment(t *testing.T, e *env, ctx context.Context, chatID, id string) {
	t.Helper()
	if err := e.st.AddDelegationObject(ctx, chatID, delegation.ContainerDeployment, id); err != nil {
		t.Fatal(err)
	}
}

// The plan builds the image and starts it: the start is covered, one approval for all three steps.
func TestProposeBuildAndDeployOneApproval(t *testing.T) {
	e := setup(t)
	got := withOriginPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, buildDeployPlan(4), true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	start := p.Steps[2]
	if !start.Covered || start.ImageOrigin == nil || start.ImageOrigin.Step != 1 || start.UncoveredReason != "" {
		t.Fatalf("start step: %+v", start)
	}
	finishSteps(t, e, ctx, p.ID, 2)
	ownDeployment(t, e, ctx, c.ID, "9")
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PUT", "/container-deployments/9/deploy", ""))
	if err != nil || r.Status != "ok" {
		t.Fatalf("start: %+v %v", r, err)
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 1 || kinds[0] != "plan" {
		t.Fatalf("approvals %v, want the plan only", kinds)
	}
	if !reached(got(), "PUT /container-deployments/9/deploy") {
		t.Fatalf("the start did not reach the platform: %v", got())
	}
}

// The demo case: a second plan "replaces" the first, whose build is done; the literal image id is traced.
func TestProposeReplacesImageFromReplacedPlan(t *testing.T) {
	e := setup(t)
	withOriginPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, buildDeployPlan(4), true); r.Status != "ok" {
		t.Fatalf("plan 1: %+v", r)
	}
	first := onlyPlan(t, e, ctx, c.ID)
	finishSteps(t, e, ctx, first.ID, 1)
	body := `{"title":"Deploy built image","replaces":"` + first.ID + `","steps":[
 {"method":"POST","path":"/container-deployments","body":{"edge_device_id":1,"container_image_id":87,"name":"agw-img","port_bindings":[{"host_port":"18119","container_port":"8000","protocol":"tcp"}]}},
 {"method":"PUT","path":"/container-deployments/{{step1.id}}/deploy"}]}`
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, body, true); r.Status != "ok" {
		t.Fatalf("plan 2: %+v", r)
	}
	var second store.Plan
	plans, err := e.st.ListPlans(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.ID != first.ID {
			if second, err = e.st.GetPlan(ctx, p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := second.Steps[1]
	if !start.Covered || start.ImageOrigin == nil || start.ImageOrigin.PlanID != first.ID || start.ImageOrigin.Step != 1 {
		t.Fatalf("start step: %+v", start)
	}
}

// A template from another repository: the start keeps its own approval and says why.
func TestProposeForeignTemplateDeployAsks(t *testing.T) {
	e := setup(t)
	withOriginPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, buildDeployPlan(9), true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	start := onlyPlan(t, e, ctx, c.ID).Steps[2]
	if start.Covered || start.ImageOrigin != nil || start.UncoveredReason != "image_origin" {
		t.Fatalf("start step: %+v", start)
	}
}

// A start outside any plan asks, also with confirm none (E2: the route keeps confirm-each).
func TestDeployOutsidePlanStillAsks(t *testing.T) {
	e := setup(t)
	withOriginPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ownDeployment(t, e, ctx, c.ID, "9")
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PUT", "/container-deployments/9/deploy", ""))
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("the start asked as %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("start: %+v", r)
	}
}

// An autonomous plan with a covered start parks nothing: the start is claimed like any other step.
func TestAutonomousCoveredDeployNotParked(t *testing.T) {
	e := setup(t)
	withOriginPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	body := strings.Replace(buildDeployPlan(4), `"title":"Build and deploy"`,
		`"title":"Build and deploy","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":5}`, 1)
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, body, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if !p.Steps[2].Covered || p.Steps[2].ParkedApproval != "" {
		t.Fatalf("start step: %+v", p.Steps[2])
	}
}

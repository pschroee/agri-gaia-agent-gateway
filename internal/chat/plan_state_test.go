// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// proposeWaiting starts a plan proposal in the background and returns its pending approval, the subscription and
// the channel of the result.
func proposeWaiting(t *testing.T, e *env, ctx context.Context, chatID, slot, body string) (store.Approval, <-chan Event, func(), <-chan platform.Result) {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, "cli", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	return ap, events, cancel, done
}

// TestPlanApprovedBeforeApprovalEvent: when Decide returns, the plan is stored as approved, and the stream carries
// the plan event before the approval event (issue #166).
func TestPlanApprovedBeforeApprovalEvent(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, events, cancel, done := proposeWaiting(t, e, ctx, c.ID, slot, planTwo)
	defer cancel()
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.State != plan.StateApproved {
		t.Fatalf("plan state right after Decide: %s", p.State)
	}
	var order []string
	timeout := time.After(3 * time.Second)
	for len(order) < 2 {
		select {
		case ev := <-events:
			switch d := ev.Data.(type) {
			case store.Plan:
				if ev.Kind == "plan" && d.State == plan.StateApproved && len(order) == 0 {
					order = append(order, "plan")
				}
			case store.Approval:
				if ev.Kind == "approval" && d.State == store.ApprovalApproved {
					order = append(order, "approval")
				}
			}
		case <-timeout:
			t.Fatalf("events: %v", order)
		}
	}
	if order[0] != "plan" || order[1] != "approval" {
		t.Fatalf("order %v, want plan before approval", order)
	}
	<-done
}

// TestPlanDecisionOnce: the outcome recorded by Decide is the one ProposePlan returns, with the time of Decide.
func TestPlanDecisionOnce(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, _, cancel, done := proposeWaiting(t, e, ctx, c.ID, slot, planTwo)
	defer cancel()
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	decidedAt := onlyPlan(t, e, ctx, c.ID).DecidedAt
	r := <-done
	if r.Status != "ok" || r.HTTPStatus != 200 || !strings.Contains(r.Body, `"state":"approved"`) || strings.Contains(r.Message, "elsewhere") {
		t.Fatalf("result: %+v", r)
	}
	if got := onlyPlan(t, e, ctx, c.ID).DecidedAt; decidedAt == nil || got == nil || !got.Equal(*decidedAt) {
		t.Fatalf("decided_at changed: %v -> %v", decidedAt, got)
	}
}

// TestPlanRejectAndExpiryStillRecorded: rejecting and letting the approval expire both store the plan state and send
// one plan event.
func TestPlanRejectAndExpiryStillRecorded(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		reject     bool
	}{{"reject", plan.StateRejected, true}, {"expire", plan.StateExpired, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			withPlanPlatform(t, e)
			if !tc.reject {
				e.m.opt.ApprovalTimeout = 100 * time.Millisecond
			}
			ctx := store.WithSession(context.Background(), "main")
			c, _ := e.m.Create(ctx, NewChat{})
			slot := e.m.live[c.ID].slot.ID
			ap, events, cancel, done := proposeWaiting(t, e, ctx, c.ID, slot, planTwo)
			defer cancel()
			if tc.reject {
				if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
					t.Fatal(err)
				}
			}
			if r := <-done; r.Status != "rejected" {
				t.Fatalf("result: %+v", r)
			}
			deadline := time.After(3 * time.Second)
			for {
				select {
				case ev := <-events:
					if p, ok := ev.Data.(store.Plan); ok && ev.Kind == "plan" && p.State == tc.want {
						if got := onlyPlan(t, e, ctx, c.ID).State; got != tc.want {
							t.Fatalf("stored state %s, want %s", got, tc.want)
						}
						return
					}
				case <-deadline:
					t.Fatalf("no plan event with state %s", tc.want)
				}
			}
		})
	}
}

// TestPlanClaimPublishesRunning: claiming a step sends the plan with the step running, before the call is answered.
func TestPlanClaimPublishesRunning(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	approvedPlan(t, e, ctx, c.ID, planTwo)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if st := e.m.planMatch(ctx, c.ID, liveCall(t, "GET", "/train/containers/7/model", "")); st == nil || st.Idx != 1 {
		t.Fatalf("match: %+v", st)
	}
	ev := waitEvent(t, events, "plan", "")
	p := ev.Data.(store.Plan)
	if len(p.Steps) == 0 || p.Steps[0].State != plan.StepRunning {
		t.Fatalf("plan event without a running step: %+v", p.Steps)
	}
}

// TestPublishKeepsPlanEvents: with a full buffer a plan event still gets through.
func TestPublishKeepsPlanEvents(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	for i := 0; i < 600; i++ {
		e.m.publish(c.ID, Event{Kind: "pi", Data: json.RawMessage(`{}`)})
	}
	e.m.publish(c.ID, Event{Kind: "plan", Data: store.Plan{ID: "marker"}})
	for {
		select {
		case ev := <-events:
			if p, ok := ev.Data.(store.Plan); ok && p.ID == "marker" {
				return
			}
		default:
			t.Fatal("plan event lost")
		}
	}
}

// B24 (Phase B #244–#251): a plan whose approval card was open when the user stopped the turn is stored as
// cancelled, like its approval, not as rejected (the Plans table said "Rejected" for a plan nobody rejected).
func TestPlanStoppedBeforeApprovalIsCancelled(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	cctx, stop := context.WithCancel(ctx)
	ap, _, cancel, done := proposeWaiting(t, e, cctx, c.ID, slot, planTwo)
	defer cancel()
	stop() // the user stopped the turn while the card was open
	if r := <-done; r.Status != "rejected" || !strings.Contains(r.Message, "stopped") {
		t.Fatalf("result: %+v", r)
	}
	if got := onlyPlan(t, e, ctx, c.ID).State; got != plan.StateCancelled {
		t.Fatalf("stored plan state %s, want %s", got, plan.StateCancelled)
	}
	if got, err := e.st.GetApproval(ctx, ap.ID); err != nil || got.State != store.ApprovalCancelled {
		t.Fatalf("approval: %+v %v", got, err)
	}
}

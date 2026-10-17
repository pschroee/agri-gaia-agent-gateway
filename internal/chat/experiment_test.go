// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// Experiment chats (X0): the binding and the protection level are properties of the chat. These tests
// run against the real store; they skip without AGW_TEST_DATABASE_URL like the other chat tests.

const experimentUser = "agent-test"

// experimentDelegation allows everything these tests need; the protection level is what the test
// varies.
const experimentDelegation = `{"rules":[
  {"action":"read","resource":"dataset","ids":["*"]},
  {"action":"delete","resource":"dataset","ids":["*"]},
  {"action":"run","resource":"training","ids":["*"]}
]}`

func experimentProfile(protection string) *store.Experiment {
	return &store.Experiment{Protection: protection, Cell: "mcp", Run: "r1", Seed: 7}
}

// allowExperiment opens the experiment for the test account.
func allowExperiment(e *env) {
	e.m.opt.ExperimentUsers = []string{"someone-else", experimentUser}
}

func createExperiment(t *testing.T, e *env, protection string, extra NewChat) ChatView {
	t.Helper()
	extra.Experiment = experimentProfile(protection)
	if len(extra.Delegation) == 0 {
		extra.Delegation = json.RawMessage(experimentDelegation)
	}
	extra.Owner, extra.OwnerName = "sub-1", experimentUser
	c, err := e.m.Create(context.Background(), extra)
	if err != nil {
		t.Fatalf("experiment chat (%s): %v", protection, err)
	}
	return c
}

// X0.2: only accounts in AGW_EXPERIMENT_USERS create experiment chats, and the name comes from the
// login, not from the request body.
func TestExperimentChatNeedsAllowedOwner(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	req := NewChat{Experiment: experimentProfile("service"), Delegation: json.RawMessage(experimentDelegation), Owner: "sub-1", OwnerName: "stranger"}
	if _, err := e.m.Create(ctx, req); !errors.Is(err, ErrExperimentNotAllowed) {
		t.Fatalf("stranger: %v", err)
	}
	allowExperiment(e)
	// The owner sub alone is not the account name the variable lists.
	if _, err := e.m.Create(ctx, req); !errors.Is(err, ErrExperimentNotAllowed) {
		t.Fatalf("sub without the login name: %v", err)
	}
	req.OwnerName = experimentUser
	c, err := e.m.Create(ctx, req)
	if err != nil {
		t.Fatalf("allowed account: %v", err)
	}
	if c.Experiment == nil || c.Experiment.Protection != "service" {
		t.Fatalf("profile not stored: %+v", c.Experiment)
	}
}

// X0.7: without delegated rights there is no protection level to map.
func TestExperimentChatNeedsDelegation(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	if _, err := e.m.Create(ctx, NewChat{Experiment: experimentProfile("none"), OwnerName: experimentUser}); !errors.Is(err, ErrExperimentNeedsDelegation) {
		t.Fatalf("without delegation: %v", err)
	}
	for _, protection := range []string{"", "strict", "SERVICE"} {
		_, err := e.m.Create(ctx, NewChat{Experiment: experimentProfile(protection), Delegation: json.RawMessage(experimentDelegation), OwnerName: experimentUser})
		if !errors.Is(err, ErrExperimentProtection) {
			t.Fatalf("protection %q: %v", protection, err)
		}
	}
}

// X0.7: none and instruction log violations, service blocks them; the confirmation is off in all
// three, whatever the request said.
func TestExperimentProtectionSetsEnforce(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	for protection, enforce := range map[string]bool{"none": false, "instruction": false, "service": true} {
		req := NewChat{Experiment: experimentProfile(protection), OwnerName: experimentUser,
			Delegation: json.RawMessage(`{"rules":[{"action":"read","resource":"dataset","ids":["*"]}],"enforce":false,"confirm":"writes"}`)}
		c, err := e.m.Create(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", protection, err)
		}
		stored, err := e.st.GetChat(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		d, err := delegation.Parse(stored.Delegation)
		if err != nil {
			t.Fatalf("%s: stored delegation: %v", protection, err)
		}
		if d.Enforcing() != enforce {
			t.Errorf("%s: enforcing %v, want %v", protection, d.Enforcing(), enforce)
		}
		if d.Confirm != "none" || d.ConfirmWrites() {
			t.Errorf("%s: confirm %q, want none", protection, d.Confirm)
		}
		if stored.Experiment == nil || stored.Experiment.Protection != protection {
			t.Errorf("%s: stored profile %+v", protection, stored.Experiment)
		}
	}
}

// X0.1: an experiment chat may name a binding that AGW_TOOLSETS does not give; the pool key carries
// the experiment prefix. An "exp:" prefix in the request is not a binding key.
func TestExperimentVariantIgnoresToolsets(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	c := createExperiment(t, e, "none", NewChat{Variant: "mcp"})
	if c.Variant != "exp:mcp" {
		t.Fatalf("stored variant %q, want exp:mcp", c.Variant)
	}
	// Without a variant the configured combination is taken, with the experiment prefix.
	c = createExperiment(t, e, "none", NewChat{})
	if c.Variant != "exp:"+e.m.opt.Toolsets.Key() {
		t.Fatalf("default variant %q", c.Variant)
	}
	if _, err := e.m.Create(ctx, NewChat{Variant: "exp:mcp", Experiment: experimentProfile("none"),
		Delegation: json.RawMessage(experimentDelegation), OwnerName: experimentUser}); !errors.Is(err, ErrUnknownVariant) {
		t.Fatalf("exp: prefix in the request: %v", err)
	}
}

// X1: the experiment runs without internet, whatever the request asks for.
func TestExperimentInternetOff(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	on := true
	c := createExperiment(t, e, "instruction", NewChat{Internet: &on})
	if c.Internet {
		t.Fatal("experiment chat with internet")
	}
	if stored, _ := e.st.GetChat(context.Background(), c.ID); stored.Internet {
		t.Fatal("internet stored")
	}
}

// The model, the provider and the proxy address stay configuration: the profile carries none of them.
func TestExperimentModelStaysConfigured(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	c := createExperiment(t, e, "none", NewChat{Model: "deepseek/klein"})
	if c.Model != "deepseek/klein" {
		t.Fatalf("model %q, want the one from the request", c.Model)
	}
	if c.Experiment != nil && c.Experiment.Protection == "" {
		t.Fatalf("profile: %+v", c.Experiment)
	}
}

// X0.9: an approval that arises despite confirm "none" (irreversible, ConfirmEach) is decided by the
// gateway, recorded as decided by the experiment, and does not wait.
func TestExperimentApprovalsAutoApproved(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	// Short enough that a waiting approval would show as expired, not as success.
	e.m.opt.ApprovalTimeout = 50 * time.Millisecond
	got := withPlatform(t, e)
	ctx := context.Background()
	c := createExperiment(t, e, "service", NewChat{Delegation: json.RawMessage(`{"rules":[
	  {"action":"delete","resource":"dataset","ids":["*"]},
	  {"action":"run","resource":"container_deployment","ids":["*"]}
	]}`)})
	slot := e.m.live[c.ID].slot.ID
	if res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: http.MethodDelete, Path: "/datasets/3"}); err != nil || res.Status != "ok" {
		t.Fatalf("irreversible call: %+v %v", res, err)
	}
	// ConfirmEach: this route asks on its own also with confirm "none" (G1.1). The deployment has to
	// come from this chat, otherwise the edge guard refuses it before any approval (D-E1).
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "1"); err != nil {
		t.Fatal(err)
	}
	if res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: http.MethodPut, Path: "/container-deployments/1/deploy"}); err != nil || res.Status != "ok" {
		t.Fatalf("confirm-each call: %+v %v", res, err)
	}
	if calls := got(); len(calls) != 2 || calls[0] != "DELETE /datasets/3" || calls[1] != "PUT /container-deployments/1/deploy" {
		t.Fatalf("platform calls: %v", calls)
	}
	aps, err := e.st.ListApprovals(ctx, "", c.ID)
	if err != nil || len(aps) != 2 {
		t.Fatalf("approvals: %+v %v", aps, err)
	}
	for _, ap := range aps {
		if ap.State != store.ApprovalApproved || ap.DecidedBy != store.DecidedByExperiment {
			t.Errorf("approval %+v", ap)
		}
		if ap.DecidedAt == nil {
			t.Error("decided_at empty")
		}
	}
	if pending, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(pending) != 0 {
		t.Errorf("pending approvals: %+v", pending)
	}
}

// X0.10: instruction and service answer the same; the enforcement does not appear.
func TestRightsTextSameForInstructionAndService(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	withPlatform(t, e)
	ctx := context.Background()
	var bodies []string
	for _, protection := range []string{"instruction", "service"} {
		c := createExperiment(t, e, protection, NewChat{})
		res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: http.MethodGet, Path: platform.RightsPath})
		if err != nil || res.Status != "ok" {
			t.Fatalf("%s: %+v %v", protection, res, err)
		}
		bodies = append(bodies, res.Body)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("rights differ between instruction and service:\n%s\n---\n%s", bodies[0], bodies[1])
	}
	for _, w := range []string{"violations are", "only logged", "blocked"} {
		if strings.Contains(bodies[0], w) {
			t.Errorf("rights name the enforcement (%q): %s", w, bodies[0])
		}
	}
	if !strings.Contains(bodies[0], "Delegated rights:") {
		t.Errorf("neutral heading missing: %s", bodies[0])
	}
}

// X0.10: in level "none" the rights tool answers like a chat without delegated rights.
func TestRightsTextNoneLikeNoDelegation(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	withPlatform(t, e)
	ctx := context.Background()
	product, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	none := createExperiment(t, e, "none", NewChat{})
	read := func(chatID string) string {
		res, err := e.m.PlatformCall(ctx, chatID, e.m.live[chatID].slot.ID, "cli", platform.Request{Method: http.MethodGet, Path: platform.RightsPath})
		if err != nil || res.Status != "ok" {
			t.Fatalf("rights: %+v %v", res, err)
		}
		return res.Body
	}
	without, got := read(product.ID), read(none.ID)
	if got != without {
		t.Fatalf("none:\n%s\n---\nwithout delegation:\n%s", got, without)
	}
	if strings.Contains(got, "experiment") || strings.Contains(got, "instruction") || strings.Contains(got, "service") {
		t.Errorf("rights name the experiment or the level: %s", got)
	}
}

// X0.12: the capability line names the binding of an experiment slot, not the profile.
func TestSetupBindingsStripExperimentPrefix(t *testing.T) {
	c := setupBindings(Setup{Variant: "exp:mcp"})
	if c.Detail != "the chat works through mcp" {
		t.Fatalf("detail %q", c.Detail)
	}
}

// X7.6/#152: the trace setting of a run reaches the slot; empty means on, "off" switches it off.
func TestExperimentTracePerRun(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	for _, tc := range []struct {
		name  string
		field string
		want  bool
	}{
		{"default", "", true},
		{"on", "on", true},
		{"off", "off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := e.m.Create(context.Background(), NewChat{Title: "trace " + tc.name,
				Experiment: &store.Experiment{Protection: "none", Cell: "cli", Run: "r1", Trace: tc.field},
				Delegation: json.RawMessage(experimentDelegation), Owner: "sub-1", OwnerName: experimentUser})
			if err != nil {
				t.Fatal(err)
			}
			if c.Experiment == nil || c.Experiment.Trace != tc.field {
				t.Fatalf("stored trace: %+v", c.Experiment)
			}
			e.mu.Lock()
			a := e.agents[len(e.agents)-1]
			e.mu.Unlock()
			calls := a.traceCalls()
			if len(calls) == 0 || calls[len(calls)-1] != tc.want {
				t.Fatalf("SetExecTrace: %v, want %v", calls, tc.want)
			}
		})
	}
	// An unknown value is refused instead of silently tracing.
	if _, err := e.m.Create(context.Background(), NewChat{Title: "bad",
		Experiment: &store.Experiment{Protection: "none", Cell: "cli", Run: "r1", Trace: "sometimes"},
		Delegation: json.RawMessage(experimentDelegation), Owner: "sub-1", OwnerName: experimentUser}); !errors.Is(err, ErrExperimentTrace) {
		t.Fatalf("invalid trace: %v", err)
	}
}

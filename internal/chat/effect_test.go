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
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// pendingApproval waits until the chat has an open approval and returns it.
func pendingApproval(t *testing.T, e *env, chatID string) store.Approval {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		aps, err := e.st.ListApprovals(context.Background(), store.ApprovalPending, chatID)
		if err != nil {
			t.Fatal(err)
		}
		if len(aps) > 0 {
			return aps[len(aps)-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no approval asked")
	return store.Approval{}
}

// startCall runs a platform call in the background; the channel gets its result.
func startCall(e *env, chatID, slot string, req platform.Request) <-chan platform.Result {
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(context.Background(), chatID, slot, "cli", req)
		done <- r
	}()
	return done
}

func TestNeedsApproval(t *testing.T) {
	none := &delegation.Delegation{Confirm: "none"}
	writes := &delegation.Delegation{}
	cases := []struct {
		name   string
		eff    delegation.Effect
		writes bool
		del    *delegation.Delegation
		want   bool
	}{
		{"read without writes", delegation.EffectRead, false, nil, false},
		{"read that writes", delegation.EffectRead, true, nil, true},
		{"write without delegation", delegation.EffectWrite, true, nil, true},
		{"write, confirm writes", delegation.EffectWrite, true, writes, true},
		{"write, confirm none", delegation.EffectWrite, true, none, false},
		{"compute without delegation", delegation.EffectCompute, true, nil, true},
		{"compute, confirm none", delegation.EffectCompute, true, none, false},
		{"irreversible without delegation", delegation.EffectIrreversible, true, nil, true},
		{"irreversible, confirm none", delegation.EffectIrreversible, true, none, true},
		{"irreversible, no write flag", delegation.EffectIrreversible, false, none, true},
	}
	for _, c := range cases {
		if got := needsApproval(c.eff, c.writes, c.del); got != c.want {
			t.Errorf("%s: needsApproval = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEffectRefusal(t *testing.T) {
	for _, eff := range []delegation.Effect{"", "delete"} {
		res, refused := effectRefusal(eff)
		if !refused || res.Status != "error" || res.Message != "refused: the gateway could not classify this call; nothing executed" {
			t.Errorf("effect %q: %+v refused=%v", eff, res, refused)
		}
	}
	for _, eff := range []delegation.Effect{delegation.EffectRead, delegation.EffectWrite, delegation.EffectCompute, delegation.EffectIrreversible} {
		if _, refused := effectRefusal(eff); refused {
			t.Errorf("effect %q must not be refused", eff)
		}
	}
}

func TestEffectPreviewTexts(t *testing.T) {
	want := map[delegation.Effect]string{
		delegation.EffectRead:         "Effect: read",
		delegation.EffectWrite:        "Effect: write (changes data on the platform)",
		delegation.EffectCompute:      "Effect: compute (uses compute time on the platform; at most 3 compute calls per chat)",
		delegation.EffectIrreversible: "Effect: irreversible (cannot be undone; every parameter is shown in full below)",
	}
	for eff, line := range want {
		if got := effectLine(eff, 3); got != line {
			t.Errorf("effectLine(%s) = %q, want %q", eff, got, line)
		}
	}
	body := json.RawMessage(`{"key":"-","csrftoken":"abcdefgh","sessionid":""}`)
	wantNote := "\n\nCVAT login fields in the body:\nCVAT field key = \"-\" (placeholder)\nCVAT field csrftoken = \"abcdefgh\"\nCVAT field sessionid = \"\" (placeholder)"
	if got := irreversibleNote(delegation.EffectIrreversible, body); got != wantNote {
		t.Errorf("irreversibleNote:\n got %q\nwant %q", got, wantNote)
	}
	if got := irreversibleNote(delegation.EffectWrite, body); got != "" {
		t.Errorf("write must add no note: %q", got)
	}
	for _, b := range []json.RawMessage{nil, json.RawMessage(`["key"]`), json.RawMessage(`{"name":"x"}`)} {
		if got := irreversibleNote(delegation.EffectIrreversible, b); got != "" {
			t.Errorf("no CVAT fields in %s: note %q", b, got)
		}
	}
	wantLimit := "refused: compute limit reached: this chat has used 3 of 3 compute calls (AGW_COMPUTE_MAX_PER_CHAT); nothing executed. A new chat has its own limit."
	if got := computeLimitResult(3); got.Status != "error" || got.Message != wantLimit {
		t.Errorf("computeLimitResult: %+v", got)
	}
}

func TestComputeMaxDefault(t *testing.T) {
	e := setup(t)
	e.m.opt.ComputeMaxPerChat = 0
	if got := e.m.computeMax(); got != 3 {
		t.Errorf("unset: %d, want 3", got)
	}
	e.m.opt.ComputeMaxPerChat = 5
	if got := e.m.computeMax(); got != 5 {
		t.Errorf("5: %d", got)
	}
}

// Irreversible calls ask the user even with confirm none; every parameter of the body is shown.
func TestIrreversibleAsksWithConfirmNone(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"delete","resource":"dataset","ids":["*"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)
	done := startCall(e, c.ID, e.m.live[c.ID].slot.ID, platform.Request{Method: "DELETE", Path: "/datasets/9", Body: body})
	ap := pendingApproval(t, e, c.ID)
	if !strings.HasPrefix(ap.Preview, "DELETE /datasets/9\nEffect: irreversible") {
		t.Fatalf("preview starts: %q", ap.Preview)
	}
	for _, w := range []string{`CVAT field key = "-" (placeholder)`, `CVAT field csrftoken = "-" (placeholder)`, `CVAT field sessionid = "-" (placeholder)`} {
		if !strings.Contains(ap.Preview, w) {
			t.Fatalf("preview without %q: %s", w, ap.Preview)
		}
	}
	if n := len(got()); n != 0 {
		t.Fatalf("executed before the decision: %v", got())
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" || len(got()) != 1 {
		t.Fatalf("approved delete: %+v %v", r, got())
	}
}

// Without a delegation the class of a call is shown and stored, and a refused call reaches no platform.
func TestEffectOfApprovals(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	cases := []struct {
		req  platform.Request
		line string
		eff  string
	}{
		{platform.Request{Method: "PATCH", Path: "/datasets/5/toggle-public"}, "Effect: irreversible (cannot be undone; every parameter is shown in full below)", "irreversible"},
		{platform.Request{Method: "DELETE", Path: "/integrated-services/3"}, "Effect: irreversible (cannot be undone; every parameter is shown in full below)", "irreversible"},
		{platform.Request{Method: "PATCH", Path: "/datasets/5", Body: json.RawMessage(`{"name":"x"}`)}, "Effect: write (changes data on the platform)", "write"},
	}
	for _, c2 := range cases {
		done := startCall(e, c.ID, slot, c2.req)
		ap := pendingApproval(t, e, c.ID)
		lines := strings.Split(ap.Preview, "\n")
		if len(lines) < 2 || lines[1] != c2.line {
			t.Fatalf("%s: preview %q, want second line %q", c2.req.Path, ap.Preview, c2.line)
		}
		if ap.Effect != c2.eff {
			t.Fatalf("%s: stored effect %q, want %q", c2.req.Path, ap.Effect, c2.eff)
		}
		if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.Status != "rejected" {
			t.Fatalf("%s: %+v", c2.req.Path, r)
		}
	}
	if n := len(got()); n != 0 {
		t.Fatalf("rejected calls reached the platform: %v", got())
	}
}

// Compute calls are limited per chat: the third is refused at once, nobody is asked. A chat with
// confirm none runs without a question and counts its own calls.
func TestComputeApprovalAndLimit(t *testing.T) {
	e := setup(t)
	e.m.opt.ComputeMaxPerChat = 2
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	run := platform.Request{Method: "POST", Path: "/train/containers/7/run"}
	for i := 0; i < 2; i++ {
		done := startCall(e, c.ID, slot, run)
		ap := pendingApproval(t, e, c.ID)
		if !strings.Contains(ap.Preview, "Effect: compute (uses compute time on the platform; at most 2 compute calls per chat)") {
			t.Fatalf("preview: %q", ap.Preview)
		}
		if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.Status != "ok" {
			t.Fatalf("run %d: %+v", i+1, r)
		}
	}
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", run)
	want := "refused: compute limit reached: this chat has used 2 of 2 compute calls (AGW_COMPUTE_MAX_PER_CHAT); nothing executed. A new chat has its own limit."
	if err != nil || r.Status != "error" || r.Message != want {
		t.Fatalf("third call: %+v %v", r, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("the refused call asked somebody: %+v", aps)
	}
	if n := len(got()); n != 2 {
		t.Fatalf("platform calls: %d, want 2", n)
	}

	c2, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"run","resource":"training","ids":["*"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := e.m.PlatformCall(ctx, c2.ID, e.m.live[c2.ID].slot.ID, "cli", run); err != nil || r.Status != "ok" {
		t.Fatalf("confirm none, no question: %+v %v", r, err)
	}
	if n, _ := e.st.ComputeCalls(ctx, c2.ID); n != 1 {
		t.Fatalf("own count of the new chat: %d", n)
	}
}

// A call the platform does not accept gives its reservation back; it does not count.
func TestComputeNotAcceptedGivesBack(t *testing.T) {
	e := setup(t)
	e.m.opt.ComputeMaxPerChat = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		if strings.Contains(r.URL.Path, "/404/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", "/tasks/6")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	pc, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = pc
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"run","resource":"training","ids":["*"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	call := func(id string) platform.Result {
		r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "POST", Path: "/train/containers/" + id + "/run"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := call("404"); r.Status == "ok" {
		t.Fatalf("404 accepted: %+v", r)
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("refused by the platform still counted: %d", n)
	}
	if r := call("7"); r.Status != "ok" {
		t.Fatalf("accepted call: %+v", r)
	}
	if r := call("8"); r.Status != "error" || !strings.Contains(r.Message, "compute limit reached") {
		t.Fatalf("over the limit: %+v", r)
	}
}

// Rights name the classes and the compute limit (the agent reads them through /_agw/rights).
func TestRightsNameEffects(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	r, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "GET", Path: platform.RightsPath})
	if err != nil || r.Status != "ok" {
		t.Fatalf("rights: %+v %v", r, err)
	}
	for _, w := range []string{"No delegation is set for this chat.", "irreversible calls (deleting, publishing) always need the user's approval", "at most 3 compute calls"} {
		if !strings.Contains(r.Body, w) {
			t.Errorf("rights without %q: %s", w, r.Body)
		}
	}
}

// The effect class is stored with the socket call (activity, issue #63).
func TestLogCallStoresEffect(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	e.m.LogCallBy(store.WithEffect(ctx, "compute"), e.m.live[c.ID].slot.ID, c.ID, "cli", "platform", "POST /train/containers/3/run", "ok 202")
	list, err := e.st.ListSocketCalls(ctx, c.ID)
	if err != nil || len(list) != 1 || list[0].Effect != "compute" {
		t.Fatalf("socket calls: %+v %v", list, err)
	}
}

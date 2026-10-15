// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// renamablePlatform answers GET /datasets/16 with a name the test can change and remembers every writing call: the
// re-read at the moment of the approval then finds another value than the proposal did (issue L2/#119).
type renamablePlatform struct {
	mu     sync.Mutex
	name   string
	fail   bool // the read of /datasets/16 answers 500
	writes []string
}

func (p *renamablePlatform) setName(n string) { p.mu.Lock(); p.name = n; p.mu.Unlock() }
func (p *renamablePlatform) setFail(v bool)   { p.mu.Lock(); p.fail = v; p.mu.Unlock() }

func (p *renamablePlatform) written() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

func withRenamablePlatform(t *testing.T, e *env) *renamablePlatform {
	t.Helper()
	p := &renamablePlatform{name: "alt"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/datasets/16" && r.Method == http.MethodGet {
			if p.fail {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"detail":"broken"}`)
				return
			}
			fmt.Fprintf(w, `{"id":16,"name":%q}`, p.name)
			return
		}
		if r.Method != http.MethodGet {
			p.writes = append(p.writes, r.Method+" "+r.URL.Path)
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return p
}

// askApproval raises a write approval for one platform call and hands back the approval and the result channel.
func askApproval(t *testing.T, e *env, ctx context.Context, chatID, slot string, req platform.Request) (store.Approval, <-chan platform.Result) {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, "cli", req)
		done <- r
	}()
	// Wait for the approval event, not for the row in the database: the event goes out after the recheck
	// of the summary is registered, so a decision here cannot race that registration.
	ap, _ := waitEvent(t, events, "approval", "").Data.(store.Approval)
	return ap, done
}

// patchDataset is the call every test of this file makes.
func patchDataset(t *testing.T, name string) platform.Request {
	t.Helper()
	req, err := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"` + name + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// Nothing changed: the click approves as before, and the approval carries no mark of a change.
func TestDecideUnchangedSummaryApproves(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 30 * time.Second
	pl := withRenamablePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, done := askApproval(t, e, ctx, c.ID, slot, patchDataset(t, "neu"))
	if ap.Summary != `Rename dataset 16 "alt" → "neu"` {
		t.Fatalf("summary: %q", ap.Summary)
	}
	got, err := e.m.Decide(ctx, ap.ID, true)
	if err != nil || got.State != store.ApprovalApproved {
		t.Fatalf("decide: %+v %v", got, err)
	}
	if got.SummaryProposed != "" || got.SummaryChangedAt != nil {
		t.Fatalf("an unchanged summary was marked as changed: %+v", got)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("call: %+v", r)
	}
	if w := pl.written(); len(w) != 1 || w[0] != "PATCH /datasets/16" {
		t.Fatalf("writes: %v", w)
	}
	// Two entries: the proposal and the re-read at the moment of the decision.
	reads := summaryReadEntries(t, e, c.ID)
	if len(reads) != 2 || !strings.Contains(reads[0].Detail, "(proposed):") || !strings.Contains(reads[1].Detail, "(approval):") {
		t.Fatalf("summary_read entries: %+v", reads)
	}
}

// The platform changed between the proposal and the click: the approval stays open, shows the new summary next to
// the one as proposed, and the call has not gone out (DL2.1, DL2.4).
func TestDecideChangedSummaryKeepsPendingAndReplaces(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 30 * time.Second
	pl := withRenamablePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, done := askApproval(t, e, ctx, c.ID, slot, patchDataset(t, "neu"))
	pl.setName("other")

	if _, err := e.m.Decide(ctx, ap.ID, true); !errors.Is(err, ErrSummaryChanged) {
		t.Fatalf("decide: %v", err)
	}
	got, err := e.st.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ApprovalPending {
		t.Fatalf("the approval was decided anyway: %+v", got)
	}
	if got.Summary != `Rename dataset 16 "other" → "neu"` || got.SummaryProposed != `Rename dataset 16 "alt" → "neu"` || got.SummaryChangedAt == nil {
		t.Fatalf("after the change: %+v", got)
	}
	if w := pl.written(); len(w) != 0 {
		t.Fatalf("the call went out despite the change: %v", w)
	}
	// A second click approves, because nothing changed again.
	if got, err := e.m.Decide(ctx, ap.ID, true); err != nil || got.State != store.ApprovalApproved {
		t.Fatalf("second click: %+v %v", got, err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("after the approval: %+v", r)
	}
	if w := pl.written(); len(w) != 1 || w[0] != "PATCH /datasets/16" {
		t.Fatalf("writes: %v", w)
	}
}

// Every further change keeps the first proposal (DL2.4) and shows the latest state; only a click that finds no
// change approves.
func TestDecideSecondApproveAfterChange(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 30 * time.Second
	pl := withRenamablePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, done := askApproval(t, e, ctx, c.ID, slot, patchDataset(t, "neu"))

	pl.setName("zwei")
	if _, err := e.m.Decide(ctx, ap.ID, true); !errors.Is(err, ErrSummaryChanged) {
		t.Fatalf("first click: %v", err)
	}
	pl.setName("drei")
	if _, err := e.m.Decide(ctx, ap.ID, true); !errors.Is(err, ErrSummaryChanged) {
		t.Fatalf("second click: %v", err)
	}
	got, err := e.st.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != `Rename dataset 16 "drei" → "neu"` || got.SummaryProposed != `Rename dataset 16 "alt" → "neu"` {
		t.Fatalf("after two changes: %+v", got)
	}
	if got, err := e.m.Decide(ctx, ap.ID, true); err != nil || got.State != store.ApprovalApproved {
		t.Fatalf("third click: %+v %v", got, err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("call: %+v", r)
	}
}

// A platform that is not fully readable does not block the approval: the stored summary is approved, and the entry
// of the re-read carries the error (DL2.3).
func TestDecideRereadFailureApprovesWithStored(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 30 * time.Second
	pl := withRenamablePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, done := askApproval(t, e, ctx, c.ID, slot, patchDataset(t, "neu"))
	pl.setFail(true)

	got, err := e.m.Decide(ctx, ap.ID, true)
	if err != nil || got.State != store.ApprovalApproved {
		t.Fatalf("a failing re-read must not block the user: %+v %v", got, err)
	}
	if got.SummaryChangedAt != nil || got.SummaryProposed != "" {
		t.Fatalf("a failed re-read counts as a change: %+v", got)
	}
	reads := summaryReadEntries(t, e, c.ID)
	if len(reads) != 2 || reads[1].Result != "error: 1 of 1 not readable" || reads[1].Detail != "for approval "+shortApprovalID(ap.ID)+" (approval): GET /datasets/16" {
		t.Fatalf("entries: %+v", reads)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("call: %+v", r)
	}
}

// A plan is re-read step by step (DL2.2): every changed step keeps its first proposal, and the approval carries the
// summary and the preview built from the new texts.
func TestDecidePlanRechecksEverySteps(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 30 * time.Second
	pl := withRenamablePlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	body := `{"title":"Rename twice","steps":[
 {"tool":"rename_dataset","args":{"dataset_id":16,"name":"eins"}},
 {"tool":"rename_dataset","args":{"dataset_id":16,"name":"zwei"}}]}`
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	ap, done := askApproval(t, e, ctx, c.ID, slot, req)
	if ap.Kind != "plan" || ap.Summary != "Plan with 2 steps\n1. "+`Rename dataset 16 "alt" → "eins"`+"\n2. "+`Rename dataset 16 "eins" → "zwei"` {
		t.Fatalf("approval: %+v", ap)
	}
	pl.setName("other")

	if _, err := e.m.Decide(ctx, ap.ID, true); !errors.Is(err, ErrSummaryChanged) {
		t.Fatalf("decide: %v", err)
	}
	stored, err := e.m.st.PlanByApproval(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Steps[0].Summary != `Rename dataset 16 "other" → "eins"` || stored.Steps[0].SummaryProposed != `Rename dataset 16 "alt" → "eins"` {
		t.Fatalf("step 1: %+v", stored.Steps[0])
	}
	// The second step renames what the first one creates; the platform state it sees did not change, and the step
	// carries the note that its values are the ones after step 1 (#72, summary.go: SummarizePlan).
	if stored.Steps[1].Summary != `Rename dataset 16 "eins" → "zwei"`+"\nCurrent values as they will be after step 1 of this plan." {
		t.Fatalf("step 2: %+v", stored.Steps[1])
	}
	got, err := e.st.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ApprovalPending || got.SummaryProposed == "" ||
		got.Summary != "Plan with 2 steps\n1. "+`Rename dataset 16 "other" → "eins"`+"\n2. "+`Rename dataset 16 "eins" → "zwei"` {
		t.Fatalf("approval after the change: %+v", got)
	}
	if !strings.Contains(got.Preview, `Rename dataset 16 "other" → "eins"`) || strings.Contains(got.Preview, `"alt" → "eins"`) {
		t.Fatalf("preview not rebuilt: %s", got.Preview)
	}
	// The next click approves; the plan runs.
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
}

// Rejecting does not re-read: the user said no to what they saw.
func TestRejectDoesNotReread(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 30 * time.Second
	pl := withRenamablePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, done := askApproval(t, e, ctx, c.ID, slot, patchDataset(t, "neu"))
	pl.setName("other")

	got, err := e.m.Decide(ctx, ap.ID, false)
	if err != nil || got.State != store.ApprovalRejected {
		t.Fatalf("reject: %+v %v", got, err)
	}
	if got.SummaryChangedAt != nil || got.SummaryProposed != "" {
		t.Fatalf("a rejection re-read the platform: %+v", got)
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("call: %+v", r)
	}
	if w := pl.written(); len(w) != 0 {
		t.Fatalf("the call went out: %v", w)
	}
	reads := summaryReadEntries(t, e, c.ID)
	if len(reads) != 1 || !strings.Contains(reads[0].Detail, "(proposed):") {
		t.Fatalf("entries: %+v", reads)
	}
}

// Once the waiting call is gone (expired), the approval belongs to no re-read any more; a click then behaves as
// before L2.
func TestRecheckUnregisteredAfterWait(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 300 * time.Millisecond
	withRenamablePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	ap, done := askApproval(t, e, ctx, c.ID, slot, patchDataset(t, "neu"))
	if e.m.recheckFor(ap.ID) == nil {
		t.Fatal("no recheck registered while the call waits")
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("expiry: %+v", r)
	}
	if e.m.recheckFor(ap.ID) != nil {
		t.Fatal("the recheck was not unregistered")
	}
	e.m.opt.ApprovalTimeout = 30 * time.Second
	got, err := e.m.Decide(ctx, ap.ID, true)
	if err != nil || got.State != store.ApprovalExpired {
		t.Fatalf("decide after the expiry: %+v %v", got, err)
	}
}

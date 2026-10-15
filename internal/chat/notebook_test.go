// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// fakeNotebook is the notebook service of these tests: it answers the chat's virtual notebook paths
// and counts what reached it.
type fakeNotebook struct {
	mu      sync.Mutex
	calls   []platform.Request
	res     platform.Result
	stopped []string
}

func (f *fakeNotebook) Do(_ context.Context, _ string, req platform.Request) (platform.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	res := f.res
	f.mu.Unlock()
	if res.Status == "" {
		res = platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"run_id":"r20260101120000-abcd"}`}
	}
	return res, nil
}

func (f *fakeNotebook) NotebookState(context.Context, string) (bool, string) {
	return true, "notebook server agri_gaia-jupyter-1a2b3c4d (running), 2 CPUs, 8 GB, no internet, no platform access"
}

func (f *fakeNotebook) StopChat(_ context.Context, chatID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, chatID)
	return nil
}

func (f *fakeNotebook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// notebookRunReq is the built call of notebook_run with one file, as the socket hands it to the
// manager: the tool built the form and the paths, the socket read the file.
func notebookRunReq(t *testing.T, code string) platform.Request {
	t.Helper()
	tool, ok := platform.Lookup("notebook_run")
	if !ok {
		t.Fatal("the tool notebook_run is missing")
	}
	r, err := tool.Build(json.RawMessage(`{"files":["/workspace/train.py"],"entry":"train.py","dataset_ids":["17"]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Uploads = []platform.Upload{{Field: "file", Name: "train.py", Data: []byte(code), SHA256: "aa11"}}
	return r
}

// notebookTestChat creates a chat that runs the tests' notebook calls.
func notebookTestChat(t *testing.T, e *env, nb *fakeNotebook) (string, string) {
	t.Helper()
	e.m.opt.Notebook = nb
	e.m.RegisterVirtual(platform.NotebookPrefix, nb)
	c, err := e.m.Create(context.Background(), NewChat{Delegation: json.RawMessage(
		`{"rules":[{"action":"run","resource":"notebook"},{"action":"update","resource":"dataset","ids":["17"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID, e.m.live[c.ID].slot.ID
}

// A run in the notebook server asks the user even when the delegation says confirm none, and it counts
// as one compute call (G1.1, G1.2).
func TestNotebookRunAlwaysAsksWithConfirmNone(t *testing.T) {
	e := setup(t)
	withPlatform(t, e)
	nb := &fakeNotebook{}
	chatID, slot := notebookTestChat(t, e, nb)
	ctx := context.Background()

	done := startCall(e, chatID, slot, notebookRunReq(t, "print('hi')\n"))
	ap := pendingApproval(t, e, chatID)
	if ap.Effect != string(delegation.EffectCompute) {
		t.Errorf("approval effect: %q", ap.Effect)
	}
	if !strings.Contains(ap.Name, "POST /_agw/notebook/runs") {
		t.Errorf("approval name: %q", ap.Name)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.Status != "ok" {
		t.Fatalf("run after the approval: %+v", res)
	}
	if nb.count() != 1 {
		t.Errorf("runs at the notebook service: %d", nb.count())
	}
	// The control: a run that the user refuses never reaches the service.
	done = startCall(e, chatID, slot, notebookRunReq(t, "print('hi')\n"))
	ap = pendingApproval(t, e, chatID)
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.Status != "rejected" {
		t.Fatalf("refused run: %+v", res)
	}
	if nb.count() != 1 {
		t.Errorf("a refused run reached the notebook service: %d", nb.count())
	}
}

// A run counts against the compute limit of the chat like a training (AGW_COMPUTE_MAX_PER_CHAT).
func TestNotebookRunCountsCompute(t *testing.T) {
	e := setup(t)
	withPlatform(t, e)
	nb := &fakeNotebook{}
	chatID, slot := notebookTestChat(t, e, nb)
	ctx := context.Background()
	e.m.opt.ComputeMaxPerChat = 1

	done := startCall(e, chatID, slot, notebookRunReq(t, "print('hi')\n"))
	ap := pendingApproval(t, e, chatID)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.Status != "ok" {
		t.Fatalf("first run: %+v", res)
	}
	if n, err := e.st.ComputeCalls(ctx, chatID); err != nil || n != 1 {
		t.Fatalf("compute calls: %d %v", n, err)
	}
	// The limit is reached before the second run: nobody is asked, and the service sees nothing.
	res, err := e.m.PlatformCall(ctx, chatID, slot, "cli", notebookRunReq(t, "print('hi')\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || !strings.Contains(res.Message, "compute limit reached") {
		t.Fatalf("second run: %+v", res)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, chatID); len(aps) != 0 {
		t.Fatalf("the user was asked although the limit is reached: %+v", aps)
	}
	if nb.count() != 1 {
		t.Errorf("runs at the notebook service: %d", nb.count())
	}
}

// The approval of a run shows the code of the run in full, with size and SHA-256; code that does not
// fit into one approval is refused instead of cut.
func TestNotebookPreviewShowsCode(t *testing.T) {
	e := setup(t)
	withPlatform(t, e)
	nb := &fakeNotebook{}
	chatID, slot := notebookTestChat(t, e, nb)
	ctx := context.Background()

	code := "import torch\nprint('hello')\n"
	done := startCall(e, chatID, slot, notebookRunReq(t, code))
	ap := pendingApproval(t, e, chatID)
	for _, w := range []string{code, "sha256 aa11", "Runs train.py in your notebook server", "no internet",
		"Datasets unpacked before the run: 17", "at most 120 minutes"} {
		if !strings.Contains(ap.Preview, w) {
			t.Errorf("preview without %q:\n%s", w, ap.Preview)
		}
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	<-done

	// A run whose code does not fit into one approval is refused: the user only approves what they see.
	long := notebookRunReq(t, strings.Repeat("# a long line\n", 4000))
	res, err := e.m.PlatformCall(ctx, chatID, slot, "cli", long)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || !strings.Contains(res.Message, "the code is too long for one approval") {
		t.Fatalf("too long: %+v", res)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, chatID); len(aps) != 0 {
		t.Fatalf("an approval was created for code that does not fit: %+v", aps)
	}
	if nb.count() != 1 {
		t.Errorf("the refused run reached the notebook service: %d", nb.count())
	}
}

// Without a notebook service the paths answer unavailable - no approval, no compute call, nothing
// reaches the platform (G1.5, G1.13).
func TestNotebookNotConfigured(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"run","resource":"notebook"}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID

	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "GET", Path: platform.NotebookPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != platform.StatusUnavailable || !strings.Contains(res.Message, "not configured on this instance") {
		t.Fatalf("status: %+v", res)
	}
	if !strings.Contains(res.Message, "capabilities shows what this setup offers") {
		t.Errorf("the answer does not point at the overview: %q", res.Message)
	}
	// A run without the capability costs neither a compute call nor an approval.
	res, err = e.m.PlatformCall(ctx, c.ID, slot, "cli", notebookRunReq(t, "print('hi')\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != platform.StatusUnavailable {
		t.Fatalf("run without a service: %+v", res)
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Errorf("an unavailable call counted as compute: %d", n)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Errorf("an unavailable call asked the user: %+v", aps)
	}
	if n := got(); len(n) != 0 {
		t.Errorf("an unavailable call reached the platform: %v", n)
	}
}

// The trash stops the chat's notebook server, and a chat with a notebook service reports the
// capability.
func TestNotebookTrashAndCapability(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	nb := &fakeNotebook{}
	e.m.opt.Notebook = nb
	caps := e.m.Capabilities(ctx, "chat")
	found := false
	for _, c := range caps.Capabilities {
		if c.Name != "notebook" {
			continue
		}
		found = true
		if !c.Available || !strings.Contains(c.Detail, "2 CPUs") || !strings.Contains(c.Without, "sandbox") {
			t.Errorf("notebook capability: %+v", c)
		}
	}
	if !found {
		t.Fatal("the overview without the notebook entry")
	}
	// Without a service the entry says why, with the way out.
	// Another chat id: the answer of a capability is kept for a minute (G1.11).
	e.m.opt.Notebook = nil
	for _, c := range e.m.Capabilities(ctx, "chat-2").Capabilities {
		if c.Name == "notebook" && (c.Available || !strings.Contains(c.Detail, "not configured on this instance")) {
			t.Errorf("notebook without a service: %+v", c)
		}
	}

	// The trash stops the server of the chat (the files stay in the volume).
	e.m.opt.Notebook = nb
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Trash(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if len(nb.stopped) != 1 || nb.stopped[0] != c.ID {
		t.Errorf("servers stopped with the trash: %v", nb.stopped)
	}
}

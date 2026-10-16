// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agw/internal/delegation"
	"agw/internal/notebook"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// fakeNotebook is the notebook service of these tests: it answers the chat's virtual notebook paths
// and counts what reached it. The structured data of an approval is built by the real service (own code
// C-E4), so a test of the approval shows what the gateway would write into it.
type fakeNotebook struct {
	mu      sync.Mutex
	calls   []platform.Request
	res     platform.Result
	stopped []string
	real    *notebook.Notebooks
}

// ApprovalData answers with the data of the real service (own code C-E4): the fake itself only answers
// the notebook paths.
func (f *fakeNotebook) ApprovalData(ctx context.Context, req platform.Request, read platform.Doer) (json.RawMessage, error) {
	if f.real == nil {
		f.real = notebook.New(notebook.Config{}, nil, nil, nil)
	}
	return f.real.ApprovalData(ctx, req, read)
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

// notebookRunReqWithNote is the same call with the agent's own note (own code C-E4): the note travels in
// the form and reaches the approval as the agent's text.
func notebookRunReqWithNote(t *testing.T, code, note string) platform.Request {
	t.Helper()
	req := notebookRunReq(t, code)
	req.Form["note"] = []string{note}
	return req
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

// The approval of a run carries the structured data of the card (own code C-E4): entry, every file with
// size and SHA-256 and its text, the datasets and the limits. The reads for the names of the datasets go
// through the reader of the summary, so its activity entry names every path once.
func TestNotebookRunApprovalData(t *testing.T) {
	e := setup(t)
	withNamedPlatform(t, e)
	nb := &fakeNotebook{}
	chatID, slot := notebookTestChat(t, e, nb)
	ctx := context.Background()

	code := "import torch\nprint('hello')\n"
	done := startCall(e, chatID, slot, notebookRunReq(t, code))
	ap := pendingApproval(t, e, chatID)
	if len(ap.Notebook) == 0 {
		t.Fatal("the approval of a run carries no structured data")
	}
	var data notebook.ApprovalData
	if err := json.Unmarshal(ap.Notebook, &data); err != nil {
		t.Fatalf("notebook field %s: %v", ap.Notebook, err)
	}
	if data.Entry != "train.py" {
		t.Errorf("entry: %q", data.Entry)
	}
	if len(data.Files) != 1 || data.Files[0].Name != "train.py" || data.Files[0].SHA256 != "aa11" ||
		data.Files[0].Size != int64(len(code)) {
		t.Fatalf("files: %+v", data.Files)
	}
	if data.Files[0].Text == nil || *data.Files[0].Text != code {
		t.Errorf("text of the file: %v", data.Files[0].Text)
	}
	if len(data.Datasets) != 1 || data.Datasets[0].ID != 17 {
		t.Errorf("datasets: %+v", data.Datasets)
	}
	if l := data.Limits; l.CPUs != 2 || l.MemGB != 8 || l.TimeoutMin != 120 || l.Internet {
		t.Errorf("limits: %+v", l)
	}
	// The read of the dataset serves the summary and the data; the entry names it once (issue L2/#119).
	reads := summaryReadEntries(t, e, chatID)
	if len(reads) != 1 || strings.Count(reads[0].Detail, "GET /datasets/17") != 1 {
		t.Fatalf("summary reads: %+v", reads)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	<-done
}

// The agent's own note reaches the approval (own code C-E4) and stays what it is: the card shows it as the
// agent's text, marked as not verified, and the gateway never states it as something it checked.
func TestNotebookRunApprovalCarriesTheAgentsNote(t *testing.T) {
	e := setup(t)
	withNamedPlatform(t, e)
	nb := &fakeNotebook{}
	chatID, slot := notebookTestChat(t, e, nb)
	ctx := context.Background()

	const note = "trains the baseline for the next step"
	done := startCall(e, chatID, slot, notebookRunReqWithNote(t, "print('hi')\n", note))
	ap := pendingApproval(t, e, chatID)
	var data notebook.ApprovalData
	if err := json.Unmarshal(ap.Notebook, &data); err != nil {
		t.Fatalf("notebook field %s: %v", ap.Notebook, err)
	}
	if data.Note != note {
		t.Errorf("note in the data: %q", data.Note)
	}
	// The summary names the note, it does not quote it: what the gateway states stays its own reading.
	if !strings.Contains(ap.Summary, "The agent's own note to the user (not verified) stands in the approval.") {
		t.Errorf("summary without the note line:\n%s", ap.Summary)
	}
	if strings.Contains(ap.Summary, note) {
		t.Errorf("the summary quotes the agent's text:\n%s", ap.Summary)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	<-done
}

// Only the approval of a run carries the structured data (own code C-E4); another platform call leaves
// the field empty.
func TestNotebookApprovalDataOnlyForRuns(t *testing.T) {
	e := setup(t)
	withNamedPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	req, err := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/17",
		Body: json.RawMessage(`{"name":"renamed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	done := startCall(e, c.ID, slot, req)
	ap := pendingApproval(t, e, c.ID)
	if len(ap.Notebook) != 0 {
		t.Errorf("a plain platform call carries notebook data: %s", ap.Notebook)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	<-done
}

// The stop of a notebook run at the user's request reaches the gateway itself (own code C-E4, "Stop job" of
// A-E2 D2.9): the call of the kind is a virtual path, and the stop of a job must not send it to the platform,
// where nothing is behind that path.
func TestStopNotebookJobReachesTheGateway(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	nb := &fakeNotebook{}
	chatID, _ := notebookTestChat(t, e, nb)
	// The service registers its kinds in main.go; the test registers the kind itself. Its route is not
	// registered: a route would make other tests' notebook calls start an observed job.
	watch.Register(notebook.New(notebook.Config{}, nil, nil, nil).WatchKind())
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: chatID, Kind: notebook.KindName,
		Ref: "r20260101120000-abcd"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.m.StopJob(ctx, chatID, jobID)
	if err != nil {
		t.Fatalf("StopJob: %v", err)
	}
	if job.ID != jobID || job.State != store.JobRunning {
		t.Errorf("job: %+v", job)
	}
	// The stop went to the notebook service and not to the platform.
	nb.mu.Lock()
	calls := append([]platform.Request(nil), nb.calls...)
	nb.mu.Unlock()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != platform.NotebookPrefix+"/stop" {
		t.Fatalf("calls at the notebook service: %+v", calls)
	}
	if calls := got(); len(calls) != 0 {
		t.Errorf("the stop went to the platform: %v", calls)
	}
	// The user's own route marks the job (own code C-E4): the card shows who stopped it.
	if stopped, err := e.st.GetWatchJob(ctx, chatID, jobID); err != nil || stopped.StoppedBy != store.StoppedByUser {
		t.Fatalf("stopped_by after the user's stop: %+v %v", stopped, err)
	}
}

// The agent's own stop of a run marks its job as well (own code C-E4): the call goes through the notebook
// paths like every call of the agent, and the job of the run says who stopped it.
func TestNotebookStopByTheAgentMarksTheJob(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	nb := &fakeNotebook{res: platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"state":"stopped"}`}}
	e.m.opt.Notebook = nb
	e.m.RegisterVirtual(platform.NotebookPrefix, nb)
	// The stop of the server is an "update notebook": the delegation of this chat grants it (with confirm
	// none, so that nobody is asked in the test).
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(
		`{"rules":[{"action":"run","resource":"notebook"},{"action":"update","resource":"notebook"}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	chatID, slot := c.ID, e.m.live[c.ID].slot.ID
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: chatID, Kind: notebook.KindName,
		Ref: "r20260101120000-abcd"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, chatID, slot, "cli",
		platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/stop"})
	if err != nil || res.Status != "ok" {
		t.Fatalf("the agent's stop: %+v %v", res, err)
	}
	if stopped, err := e.st.GetWatchJob(ctx, chatID, jobID); err != nil || stopped.StoppedBy != store.StoppedByAgent {
		t.Fatalf("stopped_by after the agent's stop: %+v %v", stopped, err)
	}
	if calls := got(); len(calls) != 0 {
		t.Errorf("the stop went to the platform: %v", calls)
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
	// The preview closes with the shown file, so its trailing newline is trimmed.
	for _, w := range []string{strings.TrimRight(code, "\n"), "sha256 aa11", "Runs train.py in your notebook server", "no internet",
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
	// A single file above the display cap (platform.NotebookTextMax) is noted rather than shown, so the
	// limit is reached with several files of printable size.
	long := notebookRunReq(t, "x")
	long.Uploads = nil
	for _, name := range []string{"a.py", "b.py", "c.py", "d.py"} {
		long.Uploads = append(long.Uploads, platform.Upload{Field: "file", Name: name,
			Data: []byte(strings.Repeat("# line\n", 2000)), SHA256: "aa11"})
	}
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

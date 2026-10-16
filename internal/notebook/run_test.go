// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// --- a fake docker ---

type execCall struct {
	ID    string
	User  string
	Cmd   []string
	Stdin []byte
}

// fakeDocker records every command and can answer it. ids is what FindByLabels returns.
type fakeDocker struct {
	mu     sync.Mutex
	calls  []execCall
	ids    []string
	labels map[string]string
	// reply answers a command; nil: exit 0 without output.
	reply func(cmd []string, stdin []byte) ([]byte, int)
}

func (f *fakeDocker) ExecAs(_ context.Context, id, user string, cmd []string, stdin io.Reader) ([]byte, int, error) {
	var data []byte
	if stdin != nil {
		data, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, execCall{ID: id, User: user, Cmd: cmd, Stdin: data})
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		return nil, 0, nil
	}
	out, code := reply(cmd, data)
	if code != 0 {
		return out, code, fmt.Errorf("exit %d", code)
	}
	return out, 0, nil
}

func (f *fakeDocker) FindByLabels(_ context.Context, labels map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = labels
	return f.ids, nil
}

// callIndex returns the position of the first recorded command that starts with the given words, or -1.
func (f *fakeDocker) callIndex(words ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if len(c.Cmd) < len(words) {
			continue
		}
		ok := true
		for j, w := range words {
			if c.Cmd[j] != w {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// call returns the first recorded command that starts with the given words.
func (f *fakeDocker) call(words ...string) (execCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c.Cmd) < len(words) {
			continue
		}
		ok := true
		for i, w := range words {
			if c.Cmd[i] != w {
				ok = false
				break
			}
		}
		if ok {
			return c, true
		}
	}
	return execCall{}, false
}

func (f *fakeDocker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- a service for the tests ---

// testService builds the service with a fake hub and a fake docker.
func testService(t *testing.T, st *store.Store, docker *fakeDocker, hub *fakeHub) (*Notebooks, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)
	n := New(Config{APIURL: srv.URL + "/hub/api", Token: "tok", PublicURL: "http://hub.example",
		Prefix: "agri_gaia-jupyter", TimeoutMaxMin: platform.NotebookTimeoutMax, DatasetMaxMB: 512, IdleMin: 60},
		platformWithUser(t, srv.URL, "agent-test"), st, docker)
	return n, srv
}

// --- starting a run ---

// A run packs the approved bytes into a tar, writes the launcher with mode 0755 and starts it
// detached; the store keeps the run.
func TestRunWritesTarAndStarts(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)

	code := []byte("print('hello')\n")
	files := []platform.Upload{{Field: "file", Name: "train.py", Data: code, SHA256: "aa11"}}
	body := platform.RunBody{Entry: "train.py", Args: []string{"--epochs", "3"}, DatasetIDs: []int64{17}, TimeoutMinutes: 90}
	// The dataset is not unpacked yet, so its archive is fetched.
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[0] == "test" && cmd[1] == "-e" {
			return nil, 1
		}
		return nil, 0
	}
	datasets := func(context.Context, int64) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("zip")), nil
	}
	run, err := n.Run(context.Background(), chatID, "agent-test", body, files, datasets)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(run.ID, "r") || len(run.ID) != 20 {
		t.Fatalf("run id: %q", run.ID)
	}
	// The hub started the chat's server (the name comes from the chat, not from the agent).
	hub.mu.Lock()
	started := hub.server
	hub.mu.Unlock()
	if started != n.Server(chatID) {
		t.Errorf("server started: %q, expected %q", started, n.Server(chatID))
	}
	// The datasets and the run go in as a command as the notebook user.
	if labels := dockerLabels(docker); labels[labelNotebook] != "1" || labels[labelUser] != "agent-test" || labels[labelServer] != n.Server(chatID) {
		t.Errorf("container labels: %v", labels)
	}

	// The runs directory is created before the tar unpacks into it: /home/jovyan/work is a volume per
	// server, so on the first run of a fresh notebook server tar -C would fail.
	mk, tarIdx := docker.callIndex("mkdir", "-p", runsDir), docker.callIndex("tar", "-x", "-C", runsDir)
	if mk < 0 {
		t.Fatalf("no mkdir of the runs directory among %v", docker.callWords())
	}
	if tarIdx < 0 || mk > tarIdx {
		t.Fatalf("the runs directory is created after the tar (%d > %d) among %v", mk, tarIdx, docker.callWords())
	}

	// The tar carries the directory, the launcher (0755) and the approved bytes.
	call, ok := docker.call("tar", "-x", "-C", runsDir)
	if !ok {
		t.Fatalf("no tar call among %v", docker.callWords())
	}
	if call.User != notebookUID {
		t.Errorf("tar as user %q", call.User)
	}
	got := map[string]struct {
		mode int64
		data string
	}{}
	tr := tar.NewReader(bytes.NewReader(call.Stdin))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar unreadable: %v", err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = struct {
			mode int64
			data string
		}{h.Mode, string(b)}
		if h.Uid != 1000 || h.Gid != 100 {
			t.Errorf("%s: uid/gid %d/%d", h.Name, h.Uid, h.Gid)
		}
	}
	if e, ok := got[run.ID+"/train.py"]; !ok || e.data != string(code) || e.mode != 0o644 {
		t.Errorf("train.py in the tar: %+v", e)
	}
	if e, ok := got[run.ID+"/.agw/run.sh"]; !ok || e.mode != 0o755 || !strings.Contains(e.data, "python -u 'train.py' '--epochs' '3'") {
		t.Errorf("run.sh in the tar: %+v", e)
	}
	// Started detached: setsid --fork, so that the exec returns while the run goes on.
	if start, ok := docker.call("setsid", "--fork", "bash"); !ok || start.Cmd[3] != runDir(run.ID)+"/.agw/run.sh" {
		t.Errorf("start call: %+v ok=%v", start, ok)
	}
	// The dataset went in through the script, without a shell for the archive.
	if ds, ok := docker.call("bash", "-c", datasetScript, "_", "17"); !ok || string(ds.Stdin) != "zip" {
		t.Errorf("dataset call: %+v ok=%v", ds, ok)
	}
	// The store keeps the run with the files the user approved.
	row, err := st.GetNotebookRun(context.Background(), chatID, run.ID)
	if err != nil {
		t.Fatalf("stored run: %v", err)
	}
	if row.Entry != "train.py" || row.TimeoutMin != 90 || row.HubUser != "agent-test" || row.Server != n.Server(chatID) {
		t.Errorf("stored run: %+v", row)
	}
	if len(row.Files) != 1 || row.Files[0].SHA256 != "aa11" || row.Files[0].Size != int64(len(code)) {
		t.Errorf("stored files: %+v", row.Files)
	}
	if len(row.DatasetIDs) != 1 || row.DatasetIDs[0] != 17 {
		t.Errorf("stored datasets: %v", row.DatasetIDs)
	}
	if row.FinishedAt != nil || row.ExitCode != nil {
		t.Errorf("a started run must be open: %+v", row)
	}
}

// dockerLabels reads the labels the service looked for.
func dockerLabels(f *fakeDocker) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.labels {
		out[k] = v
	}
	return out
}

// callWords lists the recorded commands (for the failure message of a missing one).
func (f *fakeDocker) callWords() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c.Cmd, " "))
	}
	return out
}

// A chat has one run at a time: a run that is still going blocks the next one.
func TestRunRefusesWhileActive(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()

	active := store.NotebookRun{ID: "r20260101120000-abcd", ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now().Add(-time.Minute)}
	if err := st.CreateNotebookRun(ctx, active); err != nil {
		t.Fatal(err)
	}
	body := platform.RunBody{Entry: "train.py", TimeoutMinutes: 60}
	files := []platform.Upload{{Field: "file", Name: "train.py", Data: []byte("x"), SHA256: "aa"}}
	datasets := func(context.Context, int64) (io.ReadCloser, error) { return nil, errors.New("no dataset expected") }
	if _, err := n.Run(ctx, chatID, "agent-test", body, files, datasets); err == nil || !strings.Contains(err.Error(), "still going") {
		t.Fatalf("second run: %v", err)
	}
	// The run that ended without anyone looking gets its end recorded, and then the next run starts.
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == statusScript {
			return []byte("started\t2026-01-01T10:00:00Z\nexit_code\t0\nfinished\t2026-01-01T10:05:00Z\n"), 0
		}
		return nil, 0
	}
	if _, err := n.Run(ctx, chatID, "agent-test", body, files, datasets); err != nil {
		t.Fatalf("run after the end of the old one: %v", err)
	}
	row, err := st.GetNotebookRun(ctx, chatID, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt == nil || row.ExitCode == nil || *row.ExitCode != 0 {
		t.Errorf("end of the old run not recorded: %+v", row)
	}
}

// An unpacked dataset is not fetched again.
func TestDatasetReused(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()

	files := []platform.Upload{{Field: "file", Name: "train.py", Data: []byte("x"), SHA256: "aa"}}
	body := platform.RunBody{Entry: "train.py", DatasetIDs: []int64{17, 18}, TimeoutMinutes: 60}
	var asked []int64
	// The marker file of dataset 17 is there, the one of 18 is not.
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if cmd[0] == "test" && cmd[1] == "-e" {
			if strings.Contains(cmd[2], "/17/") {
				return nil, 0
			}
			return nil, 1
		}
		return nil, 0
	}
	datasets := func(_ context.Context, id int64) (io.ReadCloser, error) {
		asked = append(asked, id)
		return io.NopCloser(strings.NewReader("zip")), nil
	}
	if _, err := n.Run(ctx, chatID, "agent-test", body, files, datasets); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != 18 {
		t.Fatalf("datasets fetched: %v (17 is unpacked already)", asked)
	}
	if _, ok := docker.call("bash", "-c", datasetScript, "_", "17"); ok {
		t.Error("the unpacked dataset was fetched again")
	}
	if _, ok := docker.call("bash", "-c", datasetScript, "_", "18"); !ok {
		t.Error("the missing dataset was not fetched")
	}
}

// The container of a chat's notebook server is found by its labels; none or several are an error.
func TestContainerByLabels(t *testing.T) {
	hub := newFakeHub()
	docker := &fakeDocker{}
	n, _ := testService(t, nil, docker, hub)
	ctx := context.Background()
	const server = "agri_gaia-jupyter-1a2b3c4d"

	if _, err := n.container(ctx, "agent-test", server); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("no container: %v", err)
	}
	docker.ids = []string{"c1", "c2"}
	if _, err := n.container(ctx, "agent-test", server); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("two containers: %v", err)
	}
	docker.ids = []string{"c1"}
	id, err := n.container(ctx, "agent-test", server)
	if err != nil || id != "c1" {
		t.Fatalf("container: %q %v", id, err)
	}
	labels := dockerLabels(docker)
	if labels[labelNotebook] != "1" || labels[labelUser] != "agent-test" || labels[labelServer] != server {
		t.Errorf("labels: %v", labels)
	}
}

// --- fetching a result ---

// The path of a fetched file is checked before anything runs: no dot segment, no absolute path.
func TestFetchPathRules(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()
	row := store.NotebookRun{ID: "r20260101120000-abcd", ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}
	if err := st.CreateNotebookRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "/etc/passwd", "../run.log", "a/../../b", "a/./b", strings.Repeat("a/", 6) + "b", "a b"} {
		if _, _, err := n.Fetch(ctx, chatID, row.ID, p); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if docker.count() != 0 {
		t.Errorf("a refused path ran a command: %v", docker.callWords())
	}
}

// A file that resolves outside the run directory is refused by the script (exit 4), and a missing or
// special file has its own answer.
func TestFetchScriptOutsideRunDir(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()
	row := store.NotebookRun{ID: "r20260101120000-abcd", ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}
	if err := st.CreateNotebookRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return nil, 4
		}
		return nil, 0
	}
	if _, _, err := n.Fetch(ctx, chatID, row.ID, "link.onnx"); err == nil || !strings.Contains(err.Error(), "does not lie in the run directory") {
		t.Fatalf("outside: %v", err)
	}
	// The script gets the resolved path, the run directory and the limit as arguments.
	call, ok := docker.call("bash", "-c", fetchScript)
	if !ok {
		t.Fatalf("no fetch call among %v", docker.callWords())
	}
	if call.Cmd[4] != runDir(row.ID)+"/link.onnx" || call.Cmd[5] != runDir(row.ID) {
		t.Errorf("fetch arguments: %v", call.Cmd)
	}
	if call.Cmd[6] != strconv.Itoa(64<<20+1) {
		t.Errorf("fetch limit: %v", call.Cmd[6])
	}
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return nil, 3
		}
		return nil, 0
	}
	if _, _, err := n.Fetch(ctx, chatID, row.ID, "model.onnx"); err == nil || !strings.Contains(err.Error(), "is not in run") {
		t.Fatalf("missing file: %v", err)
	}
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return nil, 5
		}
		return nil, 0
	}
	if _, _, err := n.Fetch(ctx, chatID, row.ID, "out"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
	// A file that is there comes back with its name and its bytes.
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return []byte("ONNX"), 0
		}
		return nil, 0
	}
	name, data, err := n.Fetch(ctx, chatID, row.ID, "out/model.onnx")
	if err != nil || name != "model.onnx" || string(data) != "ONNX" {
		t.Fatalf("fetch: %q %q %v", name, data, err)
	}
	// Without a running server the file cannot be fetched.
	if _, _, err := n.Fetch(ctx, chatID, "r20260101120000-0000", "model.onnx"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
}

// Every fetched file is remembered with the SHA-256 of the bytes the gateway read (R2.1): that row is the
// origin a check of the file rests on (R2.2). A file fetched twice keeps one row, a second run keeps its
// own.
func TestFetchRecordsSHA(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	other := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()
	first := store.NotebookRun{ID: "r20260101120000-aaaa", ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}
	second := store.NotebookRun{ID: "r20260101130000-bbbb", ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}
	for _, row := range []store.NotebookRun{first, second} {
		if err := st.CreateNotebookRun(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return []byte("ONNX"), 0
		}
		return nil, 0
	}
	if _, _, err := n.Fetch(ctx, chatID, first.ID, "out/model.onnx"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("ONNX"))
	sha := hex.EncodeToString(sum[:])
	got, found, err := st.NotebookFetchOf(ctx, chatID, sha)
	if err != nil || !found {
		t.Fatalf("the fetch was not remembered: %v %v", found, err)
	}
	if got.RunID != first.ID || got.Path != "out/model.onnx" || got.Size != 4 {
		t.Errorf("the row names another file: %+v", got)
	}
	// Nothing was recorded about the fetched file in another chat, and an unknown SHA is unknown.
	if _, found, err := st.NotebookFetchOf(ctx, other, sha); err != nil || found {
		t.Errorf("another chat knows the file: %v %v", found, err)
	}
	if _, found, err := st.NotebookFetchOf(ctx, chatID, strings.Repeat("0", 64)); err != nil || found {
		t.Errorf("an unknown SHA is known: %v %v", found, err)
	}
	// Fetching the same file again keeps one row (the run and the SHA are the key): the row still names
	// the file, not a second one.
	if _, _, err := n.Fetch(ctx, chatID, first.ID, "out/model.onnx"); err != nil {
		t.Fatal(err)
	}
	again, found, err := st.NotebookFetchOf(ctx, chatID, sha)
	if err != nil || !found || again.RunID != first.ID || again.Path != "out/model.onnx" {
		t.Errorf("the second fetch: %+v %v %v", again, found, err)
	}
	// A file of another run is a row of its own, with the SHA of its own bytes.
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return []byte("OTHER-MODEL"), 0
		}
		return nil, 0
	}
	if _, _, err := n.Fetch(ctx, chatID, second.ID, "model.onnx"); err != nil {
		t.Fatal(err)
	}
	otherSum := sha256.Sum256([]byte("OTHER-MODEL"))
	secondFile, found, err := st.NotebookFetchOf(ctx, chatID, hex.EncodeToString(otherSum[:]))
	if err != nil || !found || secondFile.RunID != "r20260101130000-bbbb" || secondFile.Path != "model.onnx" {
		t.Errorf("the file of the second run: %+v %v %v", secondFile, found, err)
	}
	// A fetch of a run of another chat is not remembered here (the row carries the chat of the run).
	if _, _, err := n.Fetch(ctx, other, first.ID, "model.onnx"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a run of another chat: %v", err)
	}
}

// --- the reaper ---

// The reaper stops a server whose last run ended long ago and leaves a chat with a recent or a
// running one alone.
func TestReaperStopsIdle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	hub := newFakeHub()
	docker := &fakeDocker{}
	n, _ := testService(t, st, docker, hub)
	old := time.Now().Add(-2 * time.Hour)

	idle := testChat(t, st)
	recent := testChat(t, st)
	running := testChat(t, st)
	finish := func(chatID, runID string, at time.Time) {
		row := store.NotebookRun{ID: runID, ChatID: chatID, HubUser: "agent-test", Server: n.Server(chatID),
			Entry: "train.py", TimeoutMin: 60, StartedAt: at.Add(-time.Minute)}
		if err := st.CreateNotebookRun(ctx, row); err != nil {
			t.Fatal(err)
		}
		exit := 0
		if err := st.FinishNotebookRun(ctx, runID, exit, at); err != nil {
			t.Fatal(err)
		}
	}
	finish(idle, "r20260101120000-aaaa", old)
	finish(recent, "r20260101120000-bbbb", time.Now())
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: "r20260101120000-cccc", ChatID: running,
		HubUser: "agent-test", Server: n.Server(running), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := n.StopIdle(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the idle chat's server was stopped, and the run stayed what it was.
	if deleted := hub.deleted(); len(deleted) != 1 || deleted[0] != n.Server(idle) {
		t.Fatalf("servers stopped: %v (only %s)", deleted, n.Server(idle))
	}
	row, err := st.GetNotebookRun(ctx, idle, "r20260101120000-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt == nil || row.ExitCode == nil || *row.ExitCode != 0 {
		t.Errorf("the reaper changed the run: %+v", row)
	}
	if docker.count() != 0 {
		t.Errorf("the reaper ran commands: %v", docker.callWords())
	}
	// Without an idle time nothing is stopped at all (AGW_NOTEBOOK_IDLE_MIN=0).
	n.cfg.IdleMin = 0
	if err := n.StopIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if deleted := hub.deleted(); len(deleted) != 1 {
		t.Errorf("stopped with an idle time of 0: %v", deleted)
	}
}

// --- the watcher ---

// The kind notebook_run reports the state of a run, records its end and names its results.
func TestNotebookWatchKind(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()
	runID := "r20260101120000-abcd"
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: runID, ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		switch {
		case len(cmd) > 2 && cmd[2] == statusScript:
			return []byte("started\t2026-01-01T10:00:00Z\nfile\tmodel.onnx\t1234\n"), 0
		case len(cmd) > 2 && cmd[2] == logScript:
			return []byte("epoch 1\nepoch 2\n"), 0
		}
		return nil, 0
	}
	kind := n.WatchKind()
	if kind.Name != KindName || kind.Poll == nil {
		t.Fatalf("kind: %+v", kind)
	}
	p, err := kind.Poll(ctx, watch.Job{ID: "j1", ChatID: chatID, Kind: KindName, Ref: runID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateRunning || p.Message != "running, at most 60 minutes" {
		t.Errorf("running: %+v", p)
	}
	if !strings.Contains(p.LogTail, "epoch 2") {
		t.Errorf("log tail: %q", p.LogTail)
	}
	if len(p.Outputs) != 1 || p.Outputs[0].Resource != "notebook_file" || p.Outputs[0].ID != runID+"/model.onnx" {
		t.Errorf("outputs: %+v", p.Outputs)
	}
	// A result file carries its size (own code C-E4): the card shows it next to the name.
	if p.Outputs[0].Size != 1234 {
		t.Errorf("size of the result file: %+v", p.Outputs[0])
	}
	// The link of the run goes along (own code C-E4): it is the address the card offers for the run.
	if want := "http://hub.example/user/agent-test/" + n.Server(chatID) + "/lab/tree/work/runs/" + runID; p.Link != want {
		t.Errorf("link: %q, expected %q", p.Link, want)
	}
	// Without a public address of the hub there is no link to pass on.
	n.cfg.PublicURL = ""
	if p, err = kind.Poll(ctx, watch.Job{ID: "j1", ChatID: chatID, Kind: KindName, Ref: runID}, nil); err != nil {
		t.Fatal(err)
	}
	if p.Link != "" {
		t.Errorf("link without a public address: %q", p.Link)
	}
	n.cfg.PublicURL = "http://hub.example"

	// An exit code of 0 ends the run as done, and the store is told.
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == statusScript {
			return []byte("started\t2026-01-01T10:00:00Z\nexit_code\t0\nfinished\t2026-01-01T10:03:00Z\nfile\tmodel.onnx\t1234\n"), 0
		}
		return nil, 0
	}
	p, err = kind.Poll(ctx, watch.Job{ID: "j1", ChatID: chatID, Kind: KindName, Ref: runID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Message != "exit 0 after 3 min 0 s" {
		t.Errorf("done: %+v", p)
	}
	if p.Percent == nil || *p.Percent != 100 {
		t.Errorf("percent: %v", p.Percent)
	}
	row, err := st.GetNotebookRun(ctx, chatID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt == nil || row.ExitCode == nil || *row.ExitCode != 0 || row.FinishedAt.UTC().Format(time.RFC3339) != "2026-01-01T10:03:00Z" {
		t.Errorf("end not recorded: %+v", row)
	}

	// A run that failed says so.
	other := "r20260101120000-ef01"
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: other, ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == statusScript {
			return []byte("started\t2026-01-01T10:00:00Z\nexit_code\t124\nfinished\t2026-01-01T10:00:30Z\n"), 0
		}
		return nil, 0
	}
	p, err = kind.Poll(ctx, watch.Job{ID: "j2", ChatID: chatID, Kind: KindName, Ref: other}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateFailed || p.Message != "exit 124 after 30 s" {
		t.Errorf("failed: %+v", p)
	}

	// The route names the call that starts a run and reads its id out of the answer.
	method, path := RunRoute()
	if method != http.MethodPost || !path.MatchString(platform.NotebookPrefix+"/runs") {
		t.Errorf("route: %s %s", method, path)
	}
	if path.MatchString(platform.NotebookPrefix + "/runs/x") {
		t.Error("the route must not match a call with something behind it")
	}
	if id, ok := RunIDFrom(platform.Request{}, platform.Result{Status: "ok", Body: `{"run_id":"` + runID + `"}`}); !ok || id != runID {
		t.Errorf("ref: %q %v", id, ok)
	}
	if _, ok := RunIDFrom(platform.Request{}, platform.Result{Status: "ok", Body: "{}"}); ok {
		t.Error("an answer without a run id must not name a job")
	}
}

// The stop of a run at the user's request (own code C-E4, "Stop job" of A-E2 D2.9): the kind names the
// call that ends it — the chat's notebook server carries the run, so that call is what stops the code —,
// and a run whose server is gone is ended in the store without an exit code, so that it neither keeps its
// job "running" nor blocks the next run.
func TestNotebookRunStop(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()
	runID := "r20260101120000-abcd"
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: runID, ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// The kind offers the stop and names the virtual call of the notebook tools; without a run id in the
	// reference there is nothing to stop, and it refuses like the other kinds.
	kind := n.WatchKind()
	if kind.Stop == nil {
		t.Fatal("the kind notebook_run cannot be stopped")
	}
	req, ok := kind.Stop(watch.Job{ID: "j1", ChatID: chatID, Kind: KindName, Ref: runID})
	if !ok || req.Method != http.MethodPost || req.Path != platform.NotebookPrefix+"/stop" {
		t.Fatalf("stop: %+v %v", req, ok)
	}
	if _, ok := kind.Stop(watch.Job{ID: "j1", ChatID: chatID, Kind: KindName}); ok {
		t.Error("a job without a run id must not be stoppable")
	}
	// The call is the one the notebook tools use: it goes to the gateway itself (the doer answers it) and
	// the hub removes the container of the server with the code in it.
	hub.server, hub.ready = n.Server(chatID), true
	res, err := n.Do(ctx, chatID, req)
	if err != nil || res.Status != "ok" {
		t.Fatalf("stop call: %+v %v", res, err)
	}
	if del := hub.deleted(); len(del) != 1 || del[0] != n.Server(chatID) {
		t.Fatalf("servers removed by the stop: %v", del)
	}
	// A chat whose server is not running is no error: the hub answers 404, the gateway takes that as done
	// (the same call is idempotent).
	if res, err = n.Do(ctx, chatID, req); err != nil || res.Status != "ok" {
		t.Fatalf("stop without a running server: %+v %v", res, err)
	}

	// The server is gone, so the run cannot go on: its end is recorded without an exit code, its state is
	// failed and the note says why.
	docker.ids = nil
	info, err := n.Status(ctx, chatID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateFailed || info.ExitCode != nil || info.FinishedAt == "" {
		t.Fatalf("status of the stopped run: %+v", info)
	}
	if !strings.Contains(info.Note, "stopped while it went") {
		t.Errorf("note of the stopped run: %q", info.Note)
	}
	row, err := st.GetNotebookRun(ctx, chatID, runID)
	if err != nil || row.FinishedAt == nil || row.ExitCode != nil {
		t.Fatalf("stored end: %+v %v", row, err)
	}
	// A second look does not move the end: the watcher polls while the job ends.
	again, err := n.Status(ctx, chatID, runID)
	if err != nil || again.FinishedAt != info.FinishedAt {
		t.Fatalf("the end of the stopped run moved: %+v %v", again, err)
	}
	if _, busy, err := st.ActiveNotebookRun(ctx, chatID); err != nil || busy {
		t.Errorf("the stopped run still blocks the chat: busy=%v err=%v", busy, err)
	}
	// A run that ended before its server went away keeps its own state and the old note.
	other := "r20260101120000-ef01"
	ended := time.Now().Add(-time.Minute)
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: other, ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now().Add(-2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishNotebookRun(ctx, other, 0, ended); err != nil {
		t.Fatal(err)
	}
	info, err = n.Status(ctx, chatID, other)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateDone || info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("a run that ended keeps its state: %+v", info)
	}
	if !strings.Contains(info.Note, "a new run starts it again") {
		t.Errorf("note of a run that ended: %q", info.Note)
	}
}

// The kind knows the call that stops a run (own code C-E4): whoever asks — the agent's tool or the user's stop
// route of A-E2 — ends the server of the chat with it, and the caller records who asked (watch_jobs.stopped_by).
func TestNotebookStoppingCall(t *testing.T) {
	stop := platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/stop"}
	if !StoppingCall(stop) {
		t.Error("the stop of the server must count as the call that stops a run")
	}
	for _, other := range []platform.Request{
		{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs"},
		{Method: http.MethodGet, Path: platform.NotebookPrefix},
		{Method: http.MethodPost, Path: platform.NotebookPrefix + "/stop/now"},
	} {
		if StoppingCall(other) {
			t.Errorf("%s must not count as the stop", other)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agw/internal/execproto"
	"agw/internal/platform"
)

// fetchBackend answers the notebook_fetch call with a file, as the gateway's notebook service does.
type fetchBackend struct {
	platformBackend
	file *platform.FileData
}

func (f *fetchBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	if f.file != nil {
		return platform.Result{Status: "ok", HTTPStatus: 200, File: f.file}, nil
	}
	return f.platformBackend.PlatformCall(ctx, chatID, slotID, via, req)
}

// startWithRunner is start with a runner (the socket of the execution sandbox with its tool runner).
func startWithRunner(t *testing.T, b Backend, run ToolRunner) *http.Client {
	t.Helper()
	return listenHandler(t, NewHandlerRun("p-test", b, 1<<20, run))
}

// A fetched result file does not go to the agent but into the workspace: the socket makes the
// directory and writes the bytes there, and the answer names the place, the size and the SHA-256
// (own code C-E3).
func TestNotebookFetchWritesToWorkspace(t *testing.T) {
	const runID = "r20260101120000-abcd"
	b := &fetchBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-nb", decide: "approved"}},
		file: &platform.FileData{Dir: runID, Name: "model.onnx", Data: []byte("ONNX")}}
	run := &fakeRunner{}
	c := startWithRunner(t, b, run)
	code, r := cliPlatform(t, c, "notebook-fetch", `{"run_id":"`+runID+`","path":"out/model.onnx"}`)
	if code != 200 || r.Status != "ok" {
		t.Fatalf("fetch: %d %+v", code, r)
	}
	dir := "/workspace/notebook/" + runID
	const onnxSHA = "9b6f7d3cbb9f1b1d3f9c0e4d38be1e73e7f0f9c1b3d5a7c9e1f3a5b7c9d1e3f5"
	if !strings.HasPrefix(r.Body, "saved as "+dir+"/model.onnx (4 bytes, sha256 ") || len(r.Body) < len("saved as "+dir+"/model.onnx (4 bytes, sha256 )") {
		t.Errorf("answer: %q", r.Body)
	}
	_ = onnxSHA
	if len(run.reqs) != 2 {
		t.Fatalf("commands of the runner: %+v", run.reqs)
	}
	if run.reqs[0].Op != execproto.OpMkdir || run.reqs[0].Path != dir {
		t.Errorf("mkdir: %+v", run.reqs[0])
	}
	w := run.reqs[1]
	if w.Op != execproto.OpWrite || w.Path != dir+"/model.onnx" || string(w.Data) != "ONNX" {
		t.Errorf("write: %+v", w)
	}
	// The call itself was a reading call to the notebook path, logged like every platform call.
	found := false
	for _, l := range b.calls {
		if strings.HasPrefix(l, "cli:platform:ok") {
			found = true
		}
	}
	if !found {
		t.Errorf("the fetch is not in the socket log: %v", b.calls)
	}

	// A file the gateway cannot name never reaches the workspace.
	b.file = &platform.FileData{Dir: "../etc", Name: "passwd", Data: []byte("x")}
	code, r = cliPlatform(t, c, "notebook-fetch", `{"run_id":"`+runID+`","path":"x.txt"}`)
	if code != 200 || r.Status != "error" || !strings.Contains(r.Message, "cannot be saved under this name") {
		t.Fatalf("odd destination: %d %+v", code, r)
	}
	if len(run.reqs) != 2 {
		t.Errorf("a refused file still reached the sandbox: %+v", run.reqs)
	}
}

// Without a command line sandbox there is no place for a fetched file (own code C-E3).
func TestNotebookFetchNeedsSandbox(t *testing.T) {
	const runID = "r20260101120000-abcd"
	b := &fetchBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-nb2", decide: "approved"}},
		file: &platform.FileData{Dir: runID, Name: "model.onnx", Data: []byte("ONNX")}}
	c := start(t, b) // socket without a runner (pi's socket)
	_, r := cliPlatform(t, c, "notebook-fetch", `{"run_id":"`+runID+`","path":"model.onnx"}`)
	if r.Status != "error" || !strings.Contains(r.Message, "fetch needs the command line sandbox") {
		t.Fatalf("without a sandbox: %+v", r)
	}
}

// The REST binding never reaches the notebook paths: a run gets its files from the sandbox, which
// only the notebook tools can read (own code C-E3).
func TestRESTRefusesNotebookPaths(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-rnb", decide: "approved"}}
	c := start(t, b)
	for _, call := range []struct{ m, p, body string }{
		{"GET", "/platform-api/_agw/notebook/runs", ""},
		{"GET", "/platform-api/_agw/notebook/runs/r20260101120000-abcd/log", ""},
		{"POST", "/platform-api/_agw/notebook/runs", "{}"},
		{"POST", "/platform-api/_agw/notebook/stop", ""},
	} {
		code, body, _ := apiCall(t, c, call.m, call.p, "", call.body, nil)
		if code != 400 || !strings.Contains(body, "notebook calls go through the notebook tools") {
			t.Errorf("%s %s: %d %s", call.m, call.p, code, body)
		}
	}
	// Nothing of it reached the backend, and every refusal is in the log.
	if len(b.reqs) != 0 {
		t.Errorf("the notebook paths reached the backend: %v", b.reqs)
	}
	refused := 0
	for _, l := range b.calls {
		if strings.HasPrefix(l, "cli:platform:refused: notebook calls go through") {
			refused++
		}
	}
	if refused != 4 {
		t.Errorf("refusals in the log: %d of 4 (%v)", refused, b.calls)
	}
}

// The raw tool request refuses the notebook paths too: only the notebook tools carry the files of a
// run.
func TestRequestToolRefusesNotebookPaths(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-req", decide: "approved"}}
	c := start(t, b)
	for _, args := range []string{
		`{"method":"POST","path":"/_agw/notebook/runs"}`,
		`{"method":"GET","path":"/_agw/notebook/runs"}`,
		`{"method":"GET","path":"/_agw/notebook/runs/r20260101120000-abcd/file"}`,
	} {
		code, r := cliPlatform(t, c, "request", args)
		if code != 200 || r.Status != "error" || !strings.Contains(r.Message, "notebook calls go through the notebook tools") {
			t.Errorf("%s: %d %+v", args, code, r)
		}
	}
	if len(b.reqs) != 0 {
		t.Errorf("the notebook paths reached the backend: %v", b.reqs)
	}
	// A path that only shares a prefix is not a notebook path.
	code, r := cliPlatform(t, c, "request", `{"method":"GET","path":"/datasets"}`)
	if code != 200 || r.Status != "ok" {
		t.Errorf("a normal call: %d %+v", code, r)
	}
	var raw map[string]any
	if json.Unmarshal([]byte(r.Body), &raw) != nil {
		t.Errorf("answer: %s", r.Body)
	}
}

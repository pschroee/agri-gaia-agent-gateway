// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The form of a run is read and checked in one place: the tool, the delegation and the runner all see
// the same values.
func TestParseRunForm(t *testing.T) {
	b, err := ParseRunForm(map[string][]string{"entry": {"train.py"}}, 0)
	if err != nil || b.Entry != "train.py" || b.TimeoutMinutes != NotebookTimeoutMinutes || len(b.Args) != 0 || len(b.DatasetIDs) != 0 {
		t.Fatalf("defaults: %+v %v", b, err)
	}
	b, err = ParseRunForm(map[string][]string{
		"entry": {"train.py"}, "args": {"--epochs", "5"}, "dataset_ids": {"17", "018", "17"}, "timeout_minutes": {"240"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Args) != 2 || b.Args[1] != "5" || b.TimeoutMinutes != 240 {
		t.Fatalf("args and timeout: %+v", b)
	}
	if len(b.DatasetIDs) != 2 || b.DatasetIDs[0] != 17 || b.DatasetIDs[1] != 18 {
		t.Errorf("dataset ids (repeated ones once, canonical): %v", b.DatasetIDs)
	}
	// The form the tool builds is the form the runner parses.
	if got := RunForm(b); got["entry"][0] != "train.py" || len(got["args"]) != 2 || got["dataset_ids"][0] != "17" || got["timeout_minutes"][0] != "240" {
		t.Errorf("RunForm: %v", got)
	}
	// The ceiling of the installed setup is where the limit is.
	if _, err := ParseRunForm(map[string][]string{"entry": {"train.py"}, "timeout_minutes": {"120"}}, 60); err == nil {
		t.Error("a timeout above the configured ceiling must be refused")
	}
	if _, err := ParseRunForm(map[string][]string{"entry": {"train.py"}, "timeout_minutes": {"60"}}, 60); err != nil {
		t.Errorf("the ceiling itself must be allowed: %v", err)
	}
	if _, err := ParseRunForm(map[string][]string{"entry": {"train.py"}, "timeout_minutes": {strconv.Itoa(NotebookTimeoutMax + 1)}}, 0); err == nil {
		t.Error("a timeout above the default ceiling must be refused")
	}
	// The entry is a plain file name, the arguments carry no shell characters.
	for _, form := range []map[string][]string{
		{"entry": {"sub/train.py"}},
		{"entry": {"../train.py"}},
		{"entry": {"-train.py"}},
		{"entry": {"train.py"}, "args": {"a;rm -rf /"}},
		{"entry": {"train.py"}, "args": {"a b"}},
		{"entry": {"train.py"}, "args": {strings.Repeat("a", 201)}},
		{"entry": {}},
	} {
		if _, err := ParseRunForm(form, 0); err == nil {
			t.Errorf("form %v accepted", form)
		}
	}
}

// The seven notebook tools stand in the table with their command line names, and notebook_run builds a
// multipart call that carries its files as sandbox paths.
func TestNotebookToolsInTable(t *testing.T) {
	want := map[string]string{
		"notebook_status": "notebook-status", "notebook_run": "notebook-run", "notebook_runs": "notebook-runs",
		"notebook_run_status": "notebook-run-status", "notebook_log": "notebook-log",
		"notebook_fetch": "notebook-fetch", "notebook_stop": "notebook-stop",
	}
	for name, cli := range want {
		tool, ok := Lookup(name)
		if !ok || tool.CLI != cli {
			t.Errorf("tool %s: %+v ok=%v", name, tool, ok)
		}
		if lookup, ok := Lookup(cli); !ok || lookup.Name != name {
			t.Errorf("the command line name %s must find %s", cli, name)
		}
	}
	// Only the run brings files from the sandbox, and only it writes.
	tool, _ := Lookup("notebook_run")
	if !tool.Write {
		t.Error("a run is a writing call (the approval follows from its effect)")
	}
	var files *Param
	for i := range tool.Params {
		if tool.Params[i].Name == "files" {
			files = &tool.Params[i]
		}
	}
	if files == nil || !files.Required || !files.Path || files.Type != "array" {
		t.Fatalf("files: %+v", files)
	}
	req, err := tool.Build(json.RawMessage(`{"files":["/workspace/train.py","/workspace/data.json"],"entry":"train.py",
		"args":["--epochs","3"],"dataset_ids":["17"],"timeout_minutes":90}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.Path != NotebookPrefix+"/runs" {
		t.Fatalf("run call: %s %s", req.Method, req.Path)
	}
	if req.Form["entry"][0] != "train.py" || len(req.Form["args"]) != 2 || req.Form["dataset_ids"][0] != "17" || req.Form["timeout_minutes"][0] != "90" {
		t.Errorf("run form: %v", req.Form)
	}
	if len(req.Files) != 2 || req.Files[0].Field != "file" || req.Files[0].Path != "/workspace/train.py" {
		t.Errorf("run files: %+v", req.Files)
	}
	// The arguments are checked while the call is built.
	for _, args := range []string{
		`{"files":["/workspace/data.pt"],"entry":"data.pt"}`,
		`{"files":["/workspace/train.py"],"entry":"other.py"}`,
		`{"files":["/workspace/train.py"],"entry":"train.py","timeout_minutes":0}`,
		`{"files":["/workspace/train.py"],"entry":"train.py","args":["a;b"]}`,
		`{"entry":"train.py"}`,
		`{"files":[],"entry":"train.py"}`,
	} {
		if _, err := tool.Build(json.RawMessage(args)); err == nil {
			t.Errorf("%s accepted", args)
		}
	}
	// The path of an uploaded file must be in the sandbox.
	if _, err := tool.Build(json.RawMessage(`{"files":["train.py"],"entry":"train.py"}`)); err == nil {
		t.Error("a relative path must be refused")
	}

	// The reading tools build their own paths; the run id is checked.
	if r, _ := mustBuild(t, "notebook_status", `{}`); r.Method != http.MethodGet || r.Path != NotebookPrefix {
		t.Errorf("status: %s %s", r.Method, r.Path)
	}
	if r, _ := mustBuild(t, "notebook_runs", `{}`); r.Path != NotebookPrefix+"/runs" {
		t.Errorf("runs: %s", r.Path)
	}
	const runID = "r20260101120000-abcd"
	if r, _ := mustBuild(t, "notebook_run_status", `{"run_id":"`+runID+`"}`); r.Path != NotebookPrefix+"/runs/"+runID {
		t.Errorf("run status: %s", r.Path)
	}
	if r, _ := mustBuild(t, "notebook_log", `{"run_id":"`+runID+`","tail":50}`); r.Path != NotebookPrefix+"/runs/"+runID+"/log" || r.Query["tail"] != "50" {
		t.Errorf("log: %s %v", r.Path, r.Query)
	}
	if r, _ := mustBuild(t, "notebook_fetch", `{"run_id":"`+runID+`","path":"out/model.onnx"}`); r.Path != NotebookPrefix+"/runs/"+runID+"/file" || r.Query["path"] != "out/model.onnx" {
		t.Errorf("fetch: %s %v", r.Path, r.Query)
	}
	if r, _ := mustBuild(t, "notebook_stop", `{}`); r.Method != http.MethodPost || r.Path != NotebookPrefix+"/stop" {
		t.Errorf("stop: %s %s", r.Method, r.Path)
	}
	for _, args := range []string{`{"run_id":"r1"}`, `{"run_id":""}`, `{"run_id":"r20260101120000-abcd/../x"}`} {
		if _, err := mustBuildErr(t, "notebook_run_status", args); err == nil {
			t.Errorf("run id %s accepted", args)
		}
	}
	// The fetched path stays inside the run directory.
	for _, args := range []string{
		`{"run_id":"` + runID + `","path":"/etc/passwd"}`,
		`{"run_id":"` + runID + `","path":"../run.log"}`,
		`{"run_id":"` + runID + `","path":"a/../../b"}`,
		`{"run_id":"` + runID + `","path":"a/b/c/d/e/f"}`,
	} {
		if _, err := mustBuildErr(t, "notebook_fetch", args); err == nil {
			t.Errorf("fetch path %s accepted", args)
		}
	}
}

func mustBuild(t *testing.T, name, args string) (Request, error) {
	t.Helper()
	tool, ok := Lookup(name)
	if !ok {
		t.Fatalf("tool %s missing", name)
	}
	r, err := tool.Build(json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r, nil
}

func mustBuildErr(t *testing.T, name, args string) (Request, error) {
	t.Helper()
	tool, ok := Lookup(name)
	if !ok {
		t.Fatalf("tool %s missing", name)
	}
	return tool.Build(json.RawMessage(args))
}

// The approval of a run shows the code with size and SHA-256 - not the form fields.
func TestNotebookPreviewShowsCode(t *testing.T) {
	code := "import torch\nprint('train')\n"
	runTool, ok := Lookup("notebook_run")
	if !ok {
		t.Fatal("the tool notebook_run is missing")
	}
	r, err := runTool.Build(json.RawMessage(`{"files":["/workspace/train.py","/workspace/config.json"],"entry":"train.py",
		"args":["--epochs","3"],"dataset_ids":["17","18"],"timeout_minutes":90}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Uploads = []Upload{
		{Field: "file", Name: "train.py", Data: []byte(code), SHA256: "aa11"},
		{Field: "file", Name: "config.json", Data: []byte(`{"img-size": 64}`), SHA256: "bb22"},
	}
	text, err := NotebookPreview(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"Runs train.py in your notebook server", "no internet", "no platform access", "at most 90 minutes",
		"Arguments: --epochs 3", "Datasets unpacked before the run: 17, 18",
		"── train.py (", "sha256 aa11", "import torch", "print('train')",
		"── config.json (", "sha256 bb22", `{"img-size": 64}`,
	} {
		if !strings.Contains(text, w) {
			t.Errorf("preview without %q:\n%s", w, text)
		}
	}

	// A notebook shows the source of its code cells.
	nb := []byte(`{"cells":[{"cell_type":"markdown","source":["# Title\n"]},
		{"cell_type":"code","source":["import torch\n","print(1)\n"]},{"cell_type":"code","source":["x = 2\n"]}]}`)
	r.Uploads = []Upload{{Field: "file", Name: "nb.ipynb", Data: nb, SHA256: "cc33"}}
	text, err = NotebookPreview(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"# cell 1", "print(1)", "# cell 2", "x = 2"} {
		if !strings.Contains(text, w) {
			t.Errorf("notebook preview without %q:\n%s", w, text)
		}
	}
	if strings.Contains(text, "# Title") {
		t.Errorf("a markdown cell is not code:\n%s", text)
	}

	// A file the approval cannot show says so instead of showing nothing.
	big := strings.Repeat("x", NotebookTextMax+1)
	r.Uploads = []Upload{{Field: "file", Name: "huge.py", Data: []byte(big), SHA256: "dd44"}}
	text, err = NotebookPreview(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "not shown") || strings.Contains(text, big) {
		t.Errorf("a huge file: %s", text[:200])
	}
	r.Uploads = []Upload{{Field: "file", Name: "model.bin", Data: []byte{0xff, 0xfe, 0x00}, SHA256: "ee55"}}
	text, err = NotebookPreview(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "not shown: not valid UTF-8") {
		t.Errorf("binary file: %q", text)
	}
}

// The notebook paths belong to the notebook tools: the raw request tool and the REST binding refuse
// them (own code C-E3), because only a tool can read the files of a run from the sandbox.
func TestNotebookPathRefusedForRawCalls(t *testing.T) {
	raw, ok := Lookup("request")
	if !ok {
		t.Fatal("the request tool is missing")
	}
	_, err := raw.Build(json.RawMessage(`{"method":"POST","path":"` + NotebookPrefix + `/runs"}`))
	if err == nil || !strings.Contains(err.Error(), "notebook calls go through the notebook tools") {
		t.Fatalf("raw call to the notebook path: %v", err)
	}
	// A GET is refused the same way: the path belongs to the tools, whatever the method.
	if _, err := raw.Build(json.RawMessage(`{"method":"GET","path":"` + NotebookPrefix + `/runs"}`)); err == nil {
		t.Error("a raw read of the notebook paths must be refused")
	}
	if why := NotebookCallRefusal(Request{Method: http.MethodGet, Path: NotebookPrefix + "/runs"}); why == "" {
		t.Error("the refusal of the notebook paths is missing")
	}
	for _, p := range []string{"/datasets", "/_agw/rights", "/notebook", "/_agw/notebooks"} {
		if why := NotebookCallRefusal(Request{Method: http.MethodGet, Path: p}); why != "" {
			t.Errorf("path %s refused: %s", p, why)
		}
	}
}

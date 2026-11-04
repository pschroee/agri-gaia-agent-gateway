// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agw/internal/execproto"
	"agw/internal/platform"
)

// captureBackend records the requests as the backend sees them (after the files are read) and answers a
// fetch with a file.
type captureBackend struct {
	platformBackend
	cmu  sync.Mutex
	got  []platform.Request
	file *platform.FileData
}

func (c *captureBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	c.cmu.Lock()
	c.got = append(c.got, req)
	c.cmu.Unlock()
	if c.file != nil && strings.HasSuffix(req.Path, "/file") {
		return platform.Result{Status: "ok", HTTPStatus: 200, File: c.file}, nil
	}
	return c.platformBackend.PlatformCall(ctx, chatID, slotID, via, req)
}

func (c *captureBackend) last(t *testing.T) platform.Request {
	t.Helper()
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if len(c.got) == 0 {
		t.Fatal("no call reached the backend")
	}
	return c.got[len(c.got)-1]
}

// The three bindings: the same arguments reach the backend as the same call with the same files, whichever way the
// agent gave them. The content lands in the sandbox before the files are read.
func TestContentRunSameInEveryBinding(t *testing.T) {
	const code = "print('hello')"
	type via struct {
		name string
		send func(t *testing.T, b *captureBackend, run *fakeRunner)
	}
	args := map[string]any{"contents": map[string]string{"main.py": code}, "entry": "main.py", "dataset_ids": []string{"3"}}
	rawArgs, _ := json.Marshal(args)
	var seen []string
	for _, v := range []via{
		{"cli", func(t *testing.T, b *captureBackend, run *fakeRunner) {
			c := startWithRunner(t, b, run)
			if code, r := cliPlatform(t, c, "notebook-run", string(rawArgs)); code != 200 || r.Status != "ok" {
				t.Fatalf("cli: %d %+v", code, r)
			}
		}},
		{"mcp", func(t *testing.T, b *captureBackend, run *fakeRunner) {
			c := listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
			res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_notebook_run", "arguments": args})
			if res["isError"] == true {
				t.Fatalf("mcp: %v", res)
			}
		}},
		{"api", func(t *testing.T, b *captureBackend, run *fakeRunner) {
			c := listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
			status, body, _ := apiCall(t, c, "POST", "/platform-api/_agw/notebook/runs", "application/json", string(rawArgs), nil)
			if status != 200 {
				t.Fatalf("api: %d %s", status, body)
			}
		}},
	} {
		b := &captureBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-" + v.name, decide: "approved"}}}
		run := &fakeRunner{file: []byte(code)}
		v.send(t, b, run)
		req := b.last(t)
		if req.Method != "POST" || req.Path != platform.NotebookPrefix+"/runs" || req.Form["entry"][0] != "main.py" || req.Form["dataset_ids"][0] != "3" {
			t.Errorf("%s: call %s %s %v", v.name, req.Method, req.Path, req.Form)
		}
		if len(req.Files) != 1 || len(req.Uploads) != 1 || req.Uploads[0].Name != "main.py" || string(req.Uploads[0].Data) != code {
			t.Errorf("%s: files %v uploads %+v", v.name, req.Files, req.Uploads)
		}
		// The sandbox got the content first (mkdir, write), then the file was read from there.
		var ops []string
		for _, r := range run.reqs {
			ops = append(ops, r.Op)
		}
		if strings.Join(ops, ",") != "mkdir,write,read" || run.reqs[1].Path != req.Files[0].Path || string(run.reqs[1].Data) != code {
			t.Errorf("%s: sandbox commands %v", v.name, run.reqs)
		}
		// The log (socket_calls) names the call with method and path, the same in every binding.
		b.mu.Lock()
		details := strings.Join(b.details, ";")
		b.mu.Unlock()
		if !strings.Contains(details, "POST "+platform.NotebookPrefix+"/runs (multipart, 1 file)") {
			t.Errorf("%s: log %q", v.name, details)
		}
		if !strings.Contains(details, "NOTEBOOK /datasets/3") {
			t.Errorf("%s: no entry per dataset: %q", v.name, details)
		}
		seen = append(seen, req.String())
	}
	if seen[0] != seen[1] || seen[1] != seen[2] {
		t.Errorf("the bindings built different calls: %v", seen)
	}
}

// Content in an upload is the same call as sandbox files, and the delegation sees it before anything is written.
func TestContentUploadAndPrecheck(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNGdata"))
	body := `{"name":"copy","description":"d","contents":{"NOTES.txt":"line one"},"contents_base64":{"a.png":"` + png + `"}}`
	b := &captureBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-up", decide: "approved"}}}
	run := &fakeRunner{file: []byte("x")}
	c := listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	status, out, _ := apiCall(t, c, "POST", "/platform-api/_agw/uploads/datasets", "application/json", body, nil)
	if status != 200 {
		t.Fatalf("upload: %d %s", status, out)
	}
	req := b.last(t)
	if req.Method != "POST" || req.Path != "/datasets" || len(req.Files) != 2 || len(req.Uploads) != 2 || req.Form["name"][0] != "copy" {
		t.Errorf("call: %s %s files %v uploads %d", req.Method, req.Path, req.Files, len(req.Uploads))
	}
	writes := 0
	for _, r := range run.reqs {
		if r.Op == execproto.OpWrite {
			writes++
			if !strings.HasPrefix(r.Path, platform.InlineDir+"/") {
				t.Errorf("written outside the inline directory: %s", r.Path)
			}
		}
	}
	if writes != 2 {
		t.Errorf("%d writes for 2 files", writes)
	}
}

// A call the delegation refuses before the files are read does not write the content either.
type refusingBackend struct{ captureBackend }

func (r *refusingBackend) PlatformPrecheck(context.Context, string, platform.Request) (platform.Result, bool) {
	return platform.Result{Status: "denied", Message: "not in the delegated rights", Decision: platform.DecisionDeniedDelegation}, false
}

func TestContentNotWrittenWhenRefused(t *testing.T) {
	b := &refusingBackend{captureBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-ref", decide: "approved"}}}}
	run := &fakeRunner{}
	c := listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	status, out, _ := apiCall(t, c, "POST", "/platform-api/_agw/notebook/runs", "application/json",
		`{"contents":{"main.py":"print(1)"},"entry":"main.py"}`, nil)
	if status != http.StatusForbidden || !strings.Contains(out, "denied by the authorization service") {
		t.Errorf("refused call: %d %s", status, out)
	}
	if len(run.reqs) != 0 {
		t.Errorf("the sandbox was touched by a refused call: %+v", run.reqs)
	}
}

// A result file fetched with inline is shown in the answer in each binding that can show it.
func TestFetchInline(t *testing.T) {
	const runID = "r20260101120000-abcd"
	text := &platform.FileData{Dir: runID, Name: "NOTES.txt", Data: []byte("Notes on the garage camera recordings\n")}
	png := &platform.FileData{Dir: runID, Name: "f.png", Data: append([]byte("\x89PNG\r\n\x1a\n"), 1, 2, 3)}
	newBackend := func(f *platform.FileData) *captureBackend {
		return &captureBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-fi", decide: "approved"}}, file: f}
	}
	args := func(p string) map[string]any {
		return map[string]any{"run_id": runID, "path": p, "inline": true}
	}

	// MCP: text as text, an image as image content, nothing written into the sandbox.
	run := &fakeRunner{}
	b := newBackend(text)
	c := listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_notebook_fetch", "arguments": args("NOTES.txt")})
	if got := mcpText(t, res); !strings.Contains(got, "Notes on the garage camera recordings") || res["isError"] == true {
		t.Errorf("mcp text: %v %s", res["isError"], got)
	}
	if len(run.reqs) != 0 {
		t.Errorf("inline copied into the sandbox: %+v", run.reqs)
	}
	b = newBackend(png)
	c = listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "platform_notebook_fetch", "arguments": args("f.png")})
	content := res["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" || content[1].(map[string]any)["mimeType"] != "image/png" {
		t.Fatalf("mcp image: %v", content)
	}
	if data, _ := base64.StdEncoding.DecodeString(content[1].(map[string]any)["data"].(string)); string(data) != string(png.Data) {
		t.Errorf("image data: %q", data)
	}

	// REST: text in the body, an image as base64 with its type in X-Agw-Image.
	b = newBackend(text)
	c = listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	status, out, _ := apiCall(t, c, "GET", "/platform-api/_agw/notebook/runs/"+runID+"/file?path=NOTES.txt&inline=true", "", "", nil)
	if status != 200 || !strings.Contains(out, "Notes on the garage") {
		t.Errorf("rest text: %d %s", status, out)
	}
	b = newBackend(png)
	c = listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	status, out, hdr := apiCall(t, c, "GET", "/platform-api/_agw/notebook/runs/"+runID+"/file?path=f.png&inline=true", "", "", nil)
	if data, _ := base64.StdEncoding.DecodeString(out); status != 200 || hdr.Get("X-Agw-Image") != "image/png" || string(data) != string(png.Data) {
		t.Errorf("rest image: %d %q %v", status, out, hdr)
	}
	if len(run.reqs) != 0 {
		t.Errorf("inline copied into the sandbox: %+v", run.reqs)
	}

	// Command line: text works; an image has no channel in the shell's answer, the agent views it from the sandbox.
	crun := &fakeRunner{}
	b = newBackend(text)
	cc := startWithRunner(t, b, crun)
	if code, r := cliPlatform(t, cc, "notebook-fetch", `{"run_id":"`+runID+`","path":"NOTES.txt","inline":true}`); code != 200 || !strings.Contains(r.Body, "Notes on the garage") {
		t.Errorf("cli text: %d %+v", code, r)
	}
	b = newBackend(png)
	cc = startWithRunner(t, b, crun)
	if _, r := cliPlatform(t, cc, "notebook-fetch", `{"run_id":"`+runID+`","path":"f.png","inline":true}`); r.Status != "error" || !strings.Contains(r.Message, "fetch it without inline") {
		t.Errorf("cli image: %+v", r)
	}

	// Too long for the answer: refused with what to do instead; nothing is copied.
	big := &platform.FileData{Dir: runID, Name: "big.txt", Data: []byte(strings.Repeat("a", platform.ViewTextMax+1))}
	b = newBackend(big)
	c = listenHandler(t, NewPiHandler("p-test", b, 1<<20, run, &fakeRecorder{}))
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "platform_notebook_fetch", "arguments": args("big.txt")})
	if got := mcpText(t, res); res["isError"] != true || !strings.Contains(got, "at most 64 KiB") {
		t.Errorf("long text: %v %s", res["isError"], got)
	}
}

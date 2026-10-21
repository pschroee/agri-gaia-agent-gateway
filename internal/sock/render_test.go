// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/toolset"
)

// renderBackend answers every platform call with res and tells the chat's bindings (#200).
type renderBackend struct {
	platformBackend
	set toolset.Set
	res platform.Result
}

func (r *renderBackend) PlatformCall(context.Context, string, string, string, platform.Request) (platform.Result, error) {
	return r.res, nil
}

func (r *renderBackend) ChatBindings(context.Context, string) toolset.Set { return r.set }

func newRenderBackend(set toolset.Set, res platform.Result) *renderBackend {
	return &renderBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-r", decide: "approved"}}, set: set, res: res}
}

func mcpText(t *testing.T, res map[string]any) string {
	t.Helper()
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

// A note with a reference to a tool reaches each binding in the form that binding calls: agw-platform on the
// command line, the MCP tool name, the path in the REST binding (as the X-Agw-Note header).
func TestNotePerBinding(t *testing.T) {
	b := newRenderBackend(toolset.Of(toolset.CLI, toolset.MCP, toolset.API),
		platform.Result{Status: "ok", HTTPStatus: 200, Body: `[]`, Note: "follow it with {{tool:assess_status}}"})
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))

	if code, r := cliPlatform(t, exec, "datasets", `{}`); code != 200 || r.Note != "follow it with agw-platform assess-status" {
		t.Errorf("CLI: %d %q", code, r.Note)
	}
	res := mcpCall(t, pi, "tools/call", map[string]any{"name": "platform_list_datasets", "arguments": map[string]any{}})
	if txt := mcpText(t, res); !strings.Contains(txt, "follow it with mcp_platform_assess_status") || strings.Contains(txt, "{{tool:") {
		t.Errorf("MCP: %s", txt)
	}
	code, _, hdr := apiCall(t, pi, "GET", "/platform-api/datasets", "", "", nil)
	got, err := url.PathUnescape(hdr.Get("X-Agw-Note"))
	if code != 200 || err != nil || got != "follow it with GET /_agw/assess/checks/{check_id}" {
		t.Errorf("REST: %d %q %v", code, got, err)
	}
}

// The text of a refusal that Result.Text adds names the rights tool in the MCP form.
func TestDeniedTextPerBinding(t *testing.T) {
	b := newRenderBackend(toolset.Of(toolset.MCP), platform.Result{Status: "denied", Message: "not in the delegated rights: delete dataset 5"})
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	res := mcpCall(t, pi, "tools/call", map[string]any{"name": "platform_list_datasets", "arguments": map[string]any{}})
	txt := mcpText(t, res)
	if res["isError"] != true || !strings.Contains(txt, "(mcp_platform_rights shows which rights apply)") || strings.Contains(txt, "{{tool:") {
		t.Errorf("denied over MCP: %v %s", res["isError"], txt)
	}
}

// The hint for a body that is not JSON names the upload tools in the forms the chat has; a chat with the REST
// binding alone has none, and the hint says so instead of naming a tool it cannot call.
func TestRESTUploadHintInAPIOnlyChat(t *testing.T) {
	for _, c := range []struct {
		set  toolset.Set
		want string
	}{
		{toolset.Of(toolset.API), "files go through upload_dataset (not offered in this setup) and upload_model (not offered in this setup)"},
		{toolset.Of(toolset.CLI, toolset.API), "files go through agw-platform upload-dataset and agw-platform upload-model"},
	} {
		b := newRenderBackend(c.set, platform.Result{Status: "ok", HTTPStatus: 200, Body: `[]`})
		pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
		code, body, _ := apiCall(t, pi, "POST", "/platform-api/datasets", "multipart/form-data", "x", nil)
		if code != 415 || !strings.Contains(body, c.want) || strings.Contains(body, "{{tool:") {
			t.Errorf("%s: %d %s", c.set, code, body)
		}
	}
}

// The log shows the text the agent read, not the marker.
func TestLoggedResultRendered(t *testing.T) {
	b := newRenderBackend(toolset.Of(toolset.MCP), platform.Result{Status: "denied", Message: "see {{tool:rights}}"})
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	mcpCall(t, pi, "tools/call", map[string]any{"name": "platform_list_datasets", "arguments": map[string]any{}})
	b.mu.Lock()
	joined := strings.Join(b.calls, ";")
	b.mu.Unlock()
	if !strings.Contains(joined, "mcp:platform:violation blocked: see mcp_platform_rights") || strings.Contains(joined, "{{tool:") {
		t.Errorf("log: %s", joined)
	}
}

// The MCP tool list carries the descriptions with the references in MCP names.
func TestMCPToolListRendered(t *testing.T) {
	pi := listenHandler(t, NewPiHandler("p-test", &fakeBackend{}, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	res := mcpCall(t, pi, "tools/list", map[string]any{})
	found := false
	for _, tl := range res["tools"].([]any) {
		m := tl.(map[string]any)
		d, _ := m["description"].(string)
		if strings.Contains(d, "{{tool:") || strings.Contains(string(mustJSON(t, m["inputSchema"])), "{{tool:") {
			t.Errorf("%s: marker left", m["name"])
		}
		if m["name"] == "platform_create_training" && strings.Contains(d, "mcp_platform_train_options") {
			found = true
		}
	}
	if !found {
		t.Error("create_training does not name mcp_platform_train_options")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

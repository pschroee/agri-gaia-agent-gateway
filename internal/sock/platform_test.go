package sock

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

// platformBackend ist fakeBackend mit Plattform: GET geht durch, Schreibendes
// entscheidet decide.
type platformBackend struct {
	fakeBackend
	reqs []string
}

func (p *platformBackend) PlatformCall(_ context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, chatID+"|"+via+"|"+req.String()+"|"+string(req.Body))
	if req.Writes() && p.decide != "approved" {
		return platform.Result{Status: "rejected", Message: "vom Nutzer abgelehnt"}, nil
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: `[{"id":1}]`}, nil
}

func cliPlatform(t *testing.T, c *http.Client, tool, args string) (int, platform.Result) {
	t.Helper()
	resp, err := c.Post("http://agw/platform/"+tool, "application/json", strings.NewReader(args))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r platform.Result
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r
}

func TestPlatformViaCLI(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-p", decide: "rejected"}}
	c := start(t, b)
	code, r := cliPlatform(t, c, "datasets", `{"limit":2}`)
	if code != 200 || r.Status != "ok" || r.Body != `[{"id":1}]` {
		t.Fatalf("lesend: %d %+v", code, r)
	}
	code, r = cliPlatform(t, c, "start-training", `{"train_container_id":3}`)
	if code != 200 || r.Status != "rejected" {
		t.Fatalf("schreibend abgelehnt: %d %+v", code, r)
	}
	code, r = cliPlatform(t, c, "request", `{"method":"GET","path":"/urls/basic-auth"}`)
	if code != 200 || r.Status != "error" || !strings.Contains(r.Message, "gesperrt") {
		t.Fatalf("gesperrter Pfad: %d %+v", code, r)
	}
	if code, _ := cliPlatform(t, c, "gibtsnicht", `{}`); code != 404 {
		t.Fatalf("unbekanntes Werkzeug: %d", code)
	}
	want := []string{"chat-p|cli|GET /datasets?limit=2|", "chat-p|cli|POST /train/containers/3/run|"}
	if strings.Join(b.reqs, ";") != strings.Join(want, ";") {
		t.Fatalf("an das Backend: %v", b.reqs)
	}
	joined := strings.Join(b.calls, ";")
	for _, w := range []string{"cli:platform:ok 200", "cli:platform:rejected", "cli:platform:abgewiesen: "} {
		if !strings.Contains(joined, w) {
			t.Fatalf("Protokoll ohne %q: %v", w, b.calls)
		}
	}
	b.mu.Lock()
	b.chat = ""
	b.mu.Unlock()
	if code, _ := cliPlatform(t, c, "datasets", `{}`); code != http.StatusConflict || len(b.reqs) != 2 {
		t.Fatalf("unzugewiesen: %d %v", code, b.reqs)
	}
}

func TestPlatformViaMCP(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-m", decide: "approved"}}
	c := start(t, b)
	res := mcpCall(t, c, "tools/list", map[string]any{})
	names := map[string]map[string]any{}
	for _, tl := range res["tools"].([]any) {
		m := tl.(map[string]any)
		names[m["name"].(string)] = m
	}
	for _, tool := range platform.Tools {
		if names[tool.MCPName()] == nil {
			t.Fatalf("MCP-Werkzeug %s fehlt", tool.MCPName())
		}
	}
	if d := names["platform_get_dataset"]["description"].(string); d != mustTool(t, "get_dataset").Desc {
		t.Fatalf("Beschreibung weicht von der Tabelle ab: %s", d)
	}
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "platform_create_training", "arguments": map[string]any{
		"provider": "Torchvision", "architecture": "EfficientNet", "category": "Classification", "dataset_id": 2, "train_config": map[string]any{"epochs": 1}}})
	if res["isError"] == true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "HTTP 200") {
		t.Fatalf("create_training: %v", res)
	}
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "platform_get_dataset", "arguments": map[string]any{}})
	if res["isError"] != true {
		t.Fatalf("fehlendes Argument muss Fehler sein: %v", res)
	}
	if len(b.reqs) != 1 || !strings.HasPrefix(b.reqs[0], "chat-m|mcp|POST /train/config|{") {
		t.Fatalf("an das Backend: %v", b.reqs)
	}
}

func TestPlatformNotConfigured(t *testing.T) {
	b := &fakeBackend{chat: "chat-x"}
	c := start(t, b)
	_, r := cliPlatform(t, c, "datasets", `{}`)
	if r.Status != "error" || !strings.Contains(r.Message, "nicht eingerichtet") {
		t.Fatalf("ohne PlatformBackend: %+v", r)
	}
}

func mustTool(t *testing.T, name string) platform.Tool {
	t.Helper()
	tool, ok := platform.Lookup(name)
	if !ok {
		t.Fatal(name)
	}
	return tool
}

// recordingBackend merkt sich die gelesenen Dateien, die beim Backend ankommen.
type uploadBackend struct {
	platformBackend
	uploads []platform.Upload
}

func (u *uploadBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	u.mu.Lock()
	u.uploads = append(u.uploads, req.Uploads...)
	u.mu.Unlock()
	return u.platformBackend.PlatformCall(ctx, chatID, slotID, via, req)
}

func startRun(t *testing.T, b Backend, run ToolRunner) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwsock") // kurzer Pfad: Unix-Sockets höchstens 104 Zeichen (macOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := Listen(dir, NewHandlerRun("p-test", b, 1<<20, run))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 5 * time.Second}
}

func TestPlatformUploadReadsFromSandbox(t *testing.T) {
	b := &uploadBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-u", decide: "approved"}}}
	run := &fakeRunner{file: []byte("PNGDATA")}
	c := startRun(t, b, run)
	code, r := cliPlatform(t, c, "upload-dataset", `{"name":"n","description":"d","files":["/workspace/a.png","/workspace/b.png"]}`)
	if code != 200 || r.Status != "ok" {
		t.Fatalf("CLI-Upload: %d %+v", code, r)
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_upload_model", "arguments": map[string]any{
		"name": "m", "description": "d", "format": "onnx", "model_file": "/workspace/m.onnx"}})
	if res["isError"] == true {
		t.Fatalf("MCP-Upload: %v", res)
	}
	if len(b.uploads) != 3 || b.uploads[0].Name != "a.png" || string(b.uploads[2].Data) != "PNGDATA" || b.uploads[2].Field != "modelfile" || len(b.uploads[0].SHA256) != 64 {
		t.Fatalf("gelesene Dateien: %+v", b.uploads)
	}
	var paths []string
	for _, rq := range run.reqs {
		paths = append(paths, rq.Op+":"+rq.Path)
	}
	if strings.Join(paths, ",") != "read:/workspace/a.png,read:/workspace/b.png,read:/workspace/m.onnx" {
		t.Fatalf("Lesevorgänge in der Sandbox: %v", paths)
	}
}

func TestPlatformUploadMissingFile(t *testing.T) {
	b := &uploadBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-u", decide: "approved"}}}
	c := startRun(t, b, &fakeRunner{}) // file nil: ENOENT
	_, r := cliPlatform(t, c, "upload-model", `{"name":"m","description":"d","format":"onnx","model_file":"/workspace/fehlt.onnx"}`)
	if r.Status != "error" || !strings.Contains(r.Message, "/workspace/fehlt.onnx") || len(b.reqs) != 0 {
		t.Fatalf("fehlende Datei: %+v %v", r, b.reqs)
	}
	// Ohne Runner (alter Socket) keine Uploads.
	c2 := start(t, b)
	if _, r := cliPlatform(t, c2, "upload-model", `{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx"}`); r.Status != "error" || !strings.Contains(r.Message, "nicht möglich") {
		t.Fatalf("ohne Runner: %+v", r)
	}
}

func apiCall(t *testing.T, c *http.Client, method, path, ctype, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://agw"+path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(strings.Builder)
	_, _ = io.Copy(b, resp.Body)
	return resp.StatusCode, b.String(), resp.Header
}

// Schritt 2: REST-Endpunkt am Socket.
func TestPlatformAPIEndpoint(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-r", decide: "rejected"}}
	c := start(t, b) // Socket der Ausführungs-Sandbox: Weg cli
	code, body, _ := apiCall(t, c, "GET", "/platform-api/datasets?limit=2", "", "", map[string]string{"Authorization": "Bearer gestohlen"})
	if code != 200 || body != `[{"id":1}]` {
		t.Fatalf("GET: %d %s", code, body)
	}
	code, body, hdr := apiCall(t, c, "PATCH", "/platform-api/datasets/5", "application/json", `{"name":"x"}`, nil)
	if code != 403 || hdr.Get("X-Agw-Outcome") != "rejected" || !strings.Contains(body, "abgelehnt") {
		t.Fatalf("schreibend abgelehnt: %d %s", code, body)
	}
	for _, bad := range []struct {
		m, p, ct, body string
		want           int
	}{
		{"GET", "/platform-api/datasets/5%2F", "", "", 400},
		{"GET", "/platform-api/d%61tasets", "", "", 400},
		{"GET", "/platform-api/urls/basic-auth", "", "", 400},
		{"GET", "/platform-api/datasets?id=1&id=2", "", "", 400},
		{"POST", "/platform-api/datasets", "multipart/form-data; boundary=x", "--x--", 415},
	} {
		if code, body, _ := apiCall(t, c, bad.m, bad.p, bad.ct, bad.body, nil); code != bad.want {
			t.Errorf("%s %s: %d %s, erwartet %d", bad.m, bad.p, code, body, bad.want)
		}
	}
	want := []string{"chat-r|cli|GET /datasets?limit=2|", "chat-r|cli|PATCH /datasets/5|{\"name\":\"x\"}"}
	if strings.Join(b.reqs, ";") != strings.Join(want, ";") {
		t.Fatalf("an das Backend: %v", b.reqs)
	}
}

// Ein Übergriff kommt als 403 mit X-Agw-Outcome: denied zurück.
type denyBackend struct{ fakeBackend }

func (d *denyBackend) PlatformCall(context.Context, string, string, string, platform.Request) (platform.Result, error) {
	return platform.Result{Status: "denied", Message: "nicht in den übertragenen Rechten: delete dataset 5"}, nil
}

func TestPlatformAPIDenied(t *testing.T) {
	b := &denyBackend{fakeBackend{chat: "chat-d"}}
	c := start(t, b)
	code, body, hdr := apiCall(t, c, "DELETE", "/platform-api/datasets/5", "", "", nil)
	if code != 403 || hdr.Get("X-Agw-Outcome") != "denied" || !strings.Contains(body, "delete dataset 5") {
		t.Fatalf("Übergriff: %d %s", code, body)
	}
	found := false
	for _, l := range b.calls {
		if strings.HasPrefix(l, "cli:platform:übergriff abgewiesen") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Übergriff nicht protokolliert: %v", b.calls)
	}
}

// Review 5, M4 und M5: Pfadvarianten erreichen die Prüfung (und das Protokoll) statt einer
// Weiterleitung der ServeMux; %20 ist die einzige erlaubte Kodierung.
func TestPlatformAPIPathVariants(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-v", decide: "approved"}}
	c := start(t, b)
	for _, p := range []string{"/platform-api/datasets/../models/1", "/platform-api//models", "/platform-api/./datasets"} {
		code, body, _ := apiCall(t, c, "GET", p, "", "", nil)
		if code != 400 {
			t.Errorf("%s: %d %s, erwartet 400", p, code, body)
		}
	}
	logged := 0
	for _, l := range b.calls {
		if strings.HasPrefix(l, "cli:platform:abgewiesen") {
			logged++
		}
	}
	if logged != 3 {
		t.Fatalf("Pfadvarianten nicht protokolliert: %v", b.calls)
	}
	if code, body, _ := apiCall(t, c, "GET", "/platform-api/train/config/Torchvision/Mask%20R-CNN", "", "", nil); code != 200 {
		t.Fatalf("%%20: %d %s", code, body)
	}
	if !strings.Contains(strings.Join(b.reqs, ";"), "GET /train/config/Torchvision/Mask R-CNN") {
		t.Fatalf("Leerzeichen nicht dekodiert: %v", b.reqs)
	}
}

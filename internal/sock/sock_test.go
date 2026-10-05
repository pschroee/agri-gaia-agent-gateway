package sock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/store"
)

type fakeBackend struct {
	mu      sync.Mutex
	chat    string
	calls   []string
	uploads []string
	decide  string // "approved" | "rejected"
	files   map[string]string
	calls2  []string // Kennungen der Werkzeugaufrufe beim Upload (store.ToolCallFrom)
	sess    []string // Sitzungen bei Upload und Internet (store.SessionFrom)
}

func (f *fakeBackend) ChatForSlot(string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chat
}

func (f *fakeBackend) Upload(ctx context.Context, chatID, slotID, via, name string, size int64, sha string, body io.Reader) (UploadResult, error) {
	b, _ := io.ReadAll(body)
	sum := sha256.Sum256(b)
	f.mu.Lock()
	f.calls2 = append(f.calls2, store.ToolCallFrom(ctx))
	f.sess = append(f.sess, store.SessionFrom(ctx))
	f.uploads = append(f.uploads, chatID+"|"+via+"|"+name+"|"+string(b))
	f.mu.Unlock()
	return UploadResult{Status: f.decide, Name: name, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (f *fakeBackend) ListArtifacts(_ context.Context, chatID string) ([]store.Artifact, error) {
	return []store.Artifact{{ChatID: chatID, Kind: "output", Name: "a.txt", Size: 3}}, nil
}

func (f *fakeBackend) OpenArtifact(_ context.Context, chatID, kind, name string) (io.ReadCloser, int64, error) {
	v, ok := f.files[kind+"/"+name]
	if !ok {
		return nil, 0, store.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(v)), int64(len(v)), nil
}

func (f *fakeBackend) RequestInternet(ctx context.Context, chatID, slotID, via, reason string) (UploadResult, error) {
	f.mu.Lock()
	f.sess = append(f.sess, store.SessionFrom(ctx))
	f.mu.Unlock()
	f.mu.Lock()
	f.uploads = append(f.uploads, "internet|"+chatID+"|"+via+"|"+reason)
	f.mu.Unlock()
	return UploadResult{Status: f.decide, Name: "internet", Message: "x"}, nil
}

func (f *fakeBackend) LogCall(slotID, chatID, via, op, detail, result string) {
	f.mu.Lock()
	f.calls = append(f.calls, via+":"+op+":"+result)
	f.mu.Unlock()
}

func start(t *testing.T, b Backend) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := Listen(dir, NewHandler("p-test", b, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	st, err := os.Stat(filepath.Join(dir, SocketName))
	if err != nil || st.Mode().Perm()&0o006 != 0o006 {
		t.Fatalf("Socket nicht für andere Nutzer verbindbar: %v %v", st.Mode(), err)
	}
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 5 * time.Second}
}

func TestUnassignedSlotRefuses(t *testing.T) {
	b := &fakeBackend{}
	c := start(t, b)
	resp, err := c.Post("http://agw/artifacts?name=x.txt", "application/octet-stream", strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "nicht zugewiesen") {
		t.Fatalf("erwartet 409 nicht zugewiesen: %d %s", resp.StatusCode, body)
	}
	if len(b.calls) != 1 || !strings.HasPrefix(b.calls[0], "cli:upload:") {
		t.Fatalf("Aufruf nicht protokolliert: %v", b.calls)
	}
}

func TestUploadApprovedViaCLI(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: "approved"}
	c := start(t, b)
	req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name=../../x.txt", strings.NewReader("abc"))
	req.Header.Set("X-Agw-Via", "cli")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r UploadResult
	json.NewDecoder(resp.Body).Decode(&r)
	if resp.StatusCode != 200 || r.Status != "approved" || r.Name != "x.txt" {
		t.Fatalf("Antwort: %d %+v", resp.StatusCode, r)
	}
	if len(b.uploads) != 1 || b.uploads[0] != "chat-1|cli|x.txt|abc" {
		t.Fatalf("Upload: %v", b.uploads)
	}
}

func TestUploadTooLarge(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: "approved"}
	c := start(t, b)
	resp, err := c.Post("http://agw/artifacts?name=big.bin", "application/octet-stream", bytes.NewReader(make([]byte, 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("413 erwartet, bekam %d", resp.StatusCode)
	}
	if len(b.uploads) != 0 {
		t.Fatal("übergroße Datei weitergereicht")
	}
}

func TestListAndGet(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", files: map[string]string{"input/daten.csv": "a,b\n"}}
	c := start(t, b)
	resp, _ := c.Get("http://agw/artifacts")
	var list []store.Artifact
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "a.txt" {
		t.Fatalf("Liste: %+v", list)
	}
	resp, _ = c.Get("http://agw/artifacts/daten.csv?kind=input")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "a,b\n" {
		t.Fatalf("Download: %d %q", resp.StatusCode, body)
	}
	resp, _ = c.Get("http://agw/artifacts/fehlt.txt")
	if resp.StatusCode != 404 {
		t.Fatalf("404 erwartet: %d", resp.StatusCode)
	}
}

func mcpCall(t *testing.T, c *http.Client, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, "http://agw/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("MCP-Antwort kein JSON (%d): %s", resp.StatusCode, raw)
	}
	if msg["error"] != nil {
		t.Fatalf("MCP-Fehler: %v", msg["error"])
	}
	return msg["result"].(map[string]any)
}

func TestMCPToolsListWorksUnassigned(t *testing.T) {
	c := start(t, &fakeBackend{})
	mcpCall(t, c, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "0"}})
	res := mcpCall(t, c, "tools/list", map[string]any{})
	names := map[string]bool{}
	for _, tl := range res["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"ping", "list_artifacts", "upload_artifact", "request_internet"} {
		if !names[want] {
			t.Fatalf("Werkzeug %s fehlt: %v", want, names)
		}
	}
}

func TestMCPPingAndUpload(t *testing.T) {
	b := &fakeBackend{chat: "chat-9", decide: "rejected"}
	c := start(t, b)
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "ping", "arguments": map[string]any{}})
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "chat-9") || !strings.Contains(text, "p-test") {
		t.Fatalf("ping: %s", text)
	}
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "upload_artifact", "arguments": map[string]any{"name": "n.txt", "content_base64": "aGFsbG8="}})
	text = res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "abgelehnt") {
		t.Fatalf("Ablehnung nicht gemeldet: %s", text)
	}
	if len(b.uploads) != 1 || b.uploads[0] != "chat-9|mcp|n.txt|hallo" {
		t.Fatalf("Upload über MCP: %v", b.uploads)
	}
	found := false
	for _, c := range b.calls {
		if strings.HasPrefix(c, "mcp:ping:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ping nicht protokolliert: %v", b.calls)
	}
}

func TestMCPCallUnassigned(t *testing.T) {
	c := start(t, &fakeBackend{})
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "list_artifacts", "arguments": map[string]any{}})
	if res["isError"] != true {
		t.Fatalf("isError erwartet: %v", res)
	}
}

func TestInternetRequestViaCLIAndMCP(t *testing.T) {
	b := &fakeBackend{chat: "chat-5", decide: "approved"}
	c := start(t, b)
	resp, err := c.Post("http://agw/internet", "application/json", strings.NewReader(`{"reason":"pip install"}`))
	if err != nil {
		t.Fatal(err)
	}
	var r UploadResult
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Status != "approved" {
		t.Fatalf("CLI: %+v", r)
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "request_internet", "arguments": map[string]any{"reason": "Webseite"}})
	if txt := res["content"].([]any)[0].(map[string]any)["text"].(string); !strings.HasPrefix(txt, "bestätigt") {
		t.Fatalf("MCP: %s", txt)
	}
	if len(b.uploads) != 2 || b.uploads[0] != "internet|chat-5|cli|pip install" || b.uploads[1] != "internet|chat-5|mcp|Webseite" {
		t.Fatalf("Anfragen: %v", b.uploads)
	}
	// Unzugewiesen: abgewiesen, nicht weitergereicht.
	b.chat = ""
	resp, _ = c.Post("http://agw/internet", "application/json", strings.NewReader(`{"reason":"x"}`))
	if resp.StatusCode != http.StatusConflict || len(b.uploads) != 2 {
		t.Fatalf("unzugewiesen: %d %v", resp.StatusCode, b.uploads)
	}
}

// M5: Der Agent kann seinen Weg nicht umdeklarieren.
func TestViaFromEndpointNotHeader(t *testing.T) {
	b := &fakeBackend{chat: "c", decide: "approved"}
	c := start(t, b)
	req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name=a.txt", strings.NewReader("x"))
	req.Header.Set("X-Agw-Via", "mcp")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(b.uploads) != 1 || !strings.Contains(b.uploads[0], "|cli|") {
		t.Fatalf("Weg: %v", b.uploads)
	}
}

func TestInternetReasonTruncated(t *testing.T) {
	b := &fakeBackend{chat: "c", decide: "approved"}
	c := start(t, b)
	long := strings.Repeat("ä", 2000)
	resp, _ := c.Post("http://agw/internet", "application/json", strings.NewReader(`{"reason":"`+long+`"}`))
	resp.Body.Close()
	if len(b.uploads) != 1 || len([]rune(b.uploads[0])) > 600 {
		t.Fatalf("Begründung nicht gekürzt: %d Zeichen", len([]rune(b.uploads[0])))
	}
}

// Die Kennung des Werkzeugaufrufs kommt beim CLI-Upload aus dem Kopf X-Agw-Tool-Call; ungültige
// Kennungen werden ignoriert.
func TestUploadToolCallHeader(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: "approved"}
	c := start(t, b)
	for _, id := range []string{"call_7", "mit leerzeichen"} {
		req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name=x.txt", strings.NewReader("abc"))
		req.Header.Set("X-Agw-Tool-Call", id)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(b.calls2) != 2 || b.calls2[0] != "call_7" || b.calls2[1] != "" {
		t.Fatalf("Kennungen: %q", b.calls2)
	}
}

// Die Sitzung (Hauptagent oder Subagenten-Lauf) kommt bei Upload und Internet aus X-Agw-Session;
// alles, was nicht wie „main“ oder eine Laufkennung aussieht, wird verworfen.
func TestCallerSessionHeader(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: "approved"}
	c := start(t, b)
	for _, s := range []string{"main", "0a1b2c3d-4e5f-6789-abcd-ef0123456789#2", "<script>"} {
		req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name=x.txt", strings.NewReader("abc"))
		req.Header.Set("X-Agw-Session", s)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	req, _ := http.NewRequest(http.MethodPost, "http://agw/internet", strings.NewReader(`{"reason":"pip"}`))
	req.Header.Set("X-Agw-Session", "0a1b2c3d-4e5f-6789-abcd-ef0123456789")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := []string{"main", "0a1b2c3d-4e5f-6789-abcd-ef0123456789#2", "", "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}
	if strings.Join(b.sess, ",") != strings.Join(want, ",") {
		t.Fatalf("Sitzungen: %q", b.sess)
	}
}

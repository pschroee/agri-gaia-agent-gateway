package sock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	decide  string   // uploads: "stored" | "rejected"; internet: "approved" | "rejected"
	details []string // detail of every logged call
	logTool []string // tool call ID of every logged call (CallerLogger)
	files   map[string]string
	calls2  []string // tool call IDs at upload (store.ToolCallFrom)
	sess    []string // sessions at upload and internet (store.SessionFrom)
	// internetOn is the chat's switch for DisableInternet.
	internetOn bool
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

func (f *fakeBackend) DisableInternet(ctx context.Context, chatID, slotID, via string) (UploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sess = append(f.sess, store.SessionFrom(ctx))
	f.uploads = append(f.uploads, "internet_off|"+chatID+"|"+via)
	if !f.internetOn {
		return UploadResult{Status: InternetAlreadyOff, Name: "internet", Message: "internet access was already off"}, nil
	}
	f.internetOn = false
	return UploadResult{Status: InternetOff, Name: "internet", Message: "internet access switched off"}, nil
}

func (f *fakeBackend) LogCall(slotID, chatID, via, op, detail, result string) {
	f.mu.Lock()
	f.calls = append(f.calls, via+":"+op+":"+result)
	f.details = append(f.details, detail)
	f.mu.Unlock()
}

func (f *fakeBackend) LogCallBy(ctx context.Context, slotID, chatID, via, op, detail, result string) {
	f.mu.Lock()
	f.logTool = append(f.logTool, store.ToolCallFrom(ctx))
	f.mu.Unlock()
	f.LogCall(slotID, chatID, via, op, detail, result)
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
		t.Fatalf("socket not connectable for other users: %v %v", st.Mode(), err)
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
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "not assigned") {
		t.Fatalf("expected 409 not assigned: %d %s", resp.StatusCode, body)
	}
	if len(b.calls) != 1 || !strings.HasPrefix(b.calls[0], "cli:upload:") {
		t.Fatalf("call not logged: %v", b.calls)
	}
}

// Issue #62: an upload is stored at once (no approval); the agent reads one line, and the socket log names name, size,
// SHA-256 and the tool call.
func TestUploadStoredViaCLI(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: UploadStored}
	c := start(t, b)
	req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name=../../x.txt", strings.NewReader("abc"))
	req.Header.Set("X-Agw-Via", "cli")
	req.Header.Set("X-Agw-Tool-Call", "call_9")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r UploadResult
	json.NewDecoder(resp.Body).Decode(&r)
	const sum = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if resp.StatusCode != 200 || r.Status != UploadStored || r.Name != "x.txt" || !strings.HasPrefix(r.Text, `sent: "x.txt" is in the chat`) {
		t.Fatalf("response: %d %+v", resp.StatusCode, r)
	}
	if len(b.uploads) != 1 || b.uploads[0] != "chat-1|cli|x.txt|abc" {
		t.Fatalf("Upload: %v", b.uploads)
	}
	if len(b.calls) != 1 || b.calls[0] != "cli:upload:stored" || b.details[0] != "x.txt (3 bytes, sha256 "+sum+")" || b.logTool[0] != "call_9" {
		t.Fatalf("socket log: %v %v %v", b.calls, b.details, b.logTool)
	}
}

// A checksum mismatch stores nothing and says why, in the reply and in the log.
func TestUploadRejectedResultText(t *testing.T) {
	r := UploadResult{Status: UploadRejected, Name: "a.csv", Message: "checksum does not match"}
	if got := UploadResultText(r); got != `not sent: "a.csv" was not stored (checksum does not match)` {
		t.Fatalf("text: %s", got)
	}
	b := &fakeBackend{chat: "chat-1", decide: UploadRejected}
	c := start(t, b)
	resp, err := c.Post("http://agw/artifacts?name=a.csv", "text/csv", strings.NewReader("1"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(b.calls) != 1 || b.calls[0] != "cli:upload:rejected" {
		t.Fatalf("socket log: %v", b.calls)
	}
}

func TestUploadTooLarge(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: UploadStored}
	c := start(t, b)
	resp, err := c.Post("http://agw/artifacts?name=big.bin", "application/octet-stream", bytes.NewReader(make([]byte, 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	if len(b.uploads) != 0 {
		t.Fatal("oversized file passed on")
	}
	if len(b.calls) != 1 || b.calls[0] != "cli:upload:refused: too large" {
		t.Fatalf("refusal not logged: %v", b.calls)
	}
	// MCP with base64: the same limit (1 MB in the test server)
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "upload_artifact", "arguments": map[string]any{"name": "big.bin", "content_base64": base64.StdEncoding.EncodeToString(make([]byte, 1<<20+1))}})
	if res["isError"] != true || len(b.uploads) != 0 {
		t.Fatalf("MCP: oversized file passed on: %v %v", res, b.uploads)
	}
}

func TestListAndGet(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", files: map[string]string{"input/data.csv": "a,b\n"}}
	c := start(t, b)
	resp, _ := c.Get("http://agw/artifacts")
	var list []store.Artifact
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "a.txt" {
		t.Fatalf("list: %+v", list)
	}
	resp, _ = c.Get("http://agw/artifacts/data.csv?kind=input")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "a,b\n" {
		t.Fatalf("Download: %d %q", resp.StatusCode, body)
	}
	resp, _ = c.Get("http://agw/artifacts/missing.txt")
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404: %d", resp.StatusCode)
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
		t.Fatalf("MCP response not JSON (%d): %s", resp.StatusCode, raw)
	}
	if msg["error"] != nil {
		t.Fatalf("MCP error: %v", msg["error"])
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
			t.Fatalf("tool %s missing: %v", want, names)
		}
	}
}

func TestMCPPingAndUpload(t *testing.T) {
	b := &fakeBackend{chat: "chat-9", decide: UploadStored}
	c := start(t, b)
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "ping", "arguments": map[string]any{}})
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "chat-9") || !strings.Contains(text, "p-test") {
		t.Fatalf("ping: %s", text)
	}
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "upload_artifact", "arguments": map[string]any{"name": "n.txt", "content_base64": "aGVsbG8="}})
	text = res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, `sent: "n.txt"`) || res["isError"] == true {
		t.Fatalf("upload not reported as sent: %s", text)
	}
	if len(b.uploads) != 1 || b.uploads[0] != "chat-9|mcp|n.txt|hello" {
		t.Fatalf("upload via MCP: %v", b.uploads)
	}
	found := false
	for _, c := range b.calls {
		if strings.HasPrefix(c, "mcp:ping:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ping not logged: %v", b.calls)
	}
	// the tool says the file goes to the user at once, without approval
	list := mcpCall(t, c, "tools/list", map[string]any{})
	for _, tl := range list["tools"].([]any) {
		m := tl.(map[string]any)
		if m["name"] == "upload_artifact" {
			d := m["description"].(string)
			if !strings.Contains(d, "without approval") || strings.Contains(d, "has to approve") {
				t.Fatalf("description: %s", d)
			}
		}
	}
}

func TestMCPCallUnassigned(t *testing.T) {
	c := start(t, &fakeBackend{})
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "list_artifacts", "arguments": map[string]any{}})
	if res["isError"] != true {
		t.Fatalf("expected isError: %v", res)
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
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "request_internet", "arguments": map[string]any{"reason": "website"}})
	if txt := res["content"].([]any)[0].(map[string]any)["text"].(string); !strings.HasPrefix(txt, "approved") {
		t.Fatalf("MCP: %s", txt)
	}
	if len(b.uploads) != 2 || b.uploads[0] != "internet|chat-5|cli|pip install" || b.uploads[1] != "internet|chat-5|mcp|website" {
		t.Fatalf("requests: %v", b.uploads)
	}
	// Unassigned: refused, not passed on.
	b.chat = ""
	resp, _ = c.Post("http://agw/internet", "application/json", strings.NewReader(`{"reason":"x"}`))
	if resp.StatusCode != http.StatusConflict || len(b.uploads) != 2 {
		t.Fatalf("unassigned: %d %v", resp.StatusCode, b.uploads)
	}
}

// M5: the agent cannot redeclare its channel.
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
		t.Fatalf("channel: %v", b.uploads)
	}
}

func TestInternetReasonTruncated(t *testing.T) {
	b := &fakeBackend{chat: "c", decide: "approved"}
	c := start(t, b)
	long := strings.Repeat("ä", 2000)
	resp, _ := c.Post("http://agw/internet", "application/json", strings.NewReader(`{"reason":"`+long+`"}`))
	resp.Body.Close()
	if len(b.uploads) != 1 || len([]rune(b.uploads[0])) > 600 {
		t.Fatalf("reason not truncated: %d characters", len([]rune(b.uploads[0])))
	}
}

// The ID of the tool call comes from the header X-Agw-Tool-Call for a CLI upload; invalid
// IDs are ignored.
func TestUploadToolCallHeader(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: "approved"}
	c := start(t, b)
	for _, id := range []string{"call_7", "with spaces"} {
		req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name=x.txt", strings.NewReader("abc"))
		req.Header.Set("X-Agw-Tool-Call", id)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(b.calls2) != 2 || b.calls2[0] != "call_7" || b.calls2[1] != "" {
		t.Fatalf("IDs: %q", b.calls2)
	}
}

// The session (main agent or subagent run) comes from X-Agw-Session for upload and internet;
// anything that does not look like "main" or a run ID is discarded.
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
		t.Fatalf("sessions: %q", b.sess)
	}
}

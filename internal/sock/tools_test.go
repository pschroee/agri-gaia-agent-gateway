package sock

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/execproto"
	"agw/internal/store"
)

type fakeRunner struct {
	mu   sync.Mutex
	reqs []execproto.Request
	// bash: Stücke, die nacheinander kommen; block hält bis zum Abbruch.
	chunks []string
	block  bool
	file   []byte
}

func (f *fakeRunner) Run(ctx context.Context, req execproto.Request, onData func([]byte)) (execproto.Frame, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	switch req.Op {
	case execproto.OpBash:
		for _, c := range f.chunks {
			onData([]byte(c))
		}
		if f.block {
			<-ctx.Done()
			return execproto.Frame{Done: true, Error: "aborted", Code: "aborted"}, ctx.Err()
		}
		code := 0
		return execproto.Frame{Done: true, Exit: &code, FullOutputPath: req.Spill}, nil
	case execproto.OpRead:
		if f.file == nil {
			return execproto.Frame{Done: true, Error: "open " + req.Path + ": no such file or directory", Code: "ENOENT"}, nil
		}
		b, _ := json.Marshal(execproto.ReadResult{Data: f.file, Size: int64(len(f.file))})
		return execproto.Frame{Done: true, Result: b}, nil
	}
	return execproto.Frame{Done: true, Result: json.RawMessage(`{}`)}, nil
}

type fakeRecorder struct {
	mu   sync.Mutex
	recs []store.ToolExecution
}

func (f *fakeRecorder) RecordToolExecution(e store.ToolExecution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, e)
}

func (f *fakeRecorder) all() []store.ToolExecution {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ToolExecution(nil), f.recs...)
}

func startPi(t *testing.T, b *fakeBackend, run *fakeRunner, rec *fakeRecorder) *http.Client {
	t.Helper()
	return startPiRunner(t, b, run, rec)
}

func startPiRunner(t *testing.T, b *fakeBackend, run ToolRunner, rec *fakeRecorder) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwsockpi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := Listen(dir, NewPiHandler("p-test", b, 1<<20, run, rec))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 10 * time.Second}
}

func postJSON(t *testing.T, c *http.Client, path string, v any) (*http.Response, string) {
	t.Helper()
	b, _ := json.Marshal(v)
	resp, err := c.Post("http://agw"+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		sb.WriteString(sc.Text() + "\n")
	}
	return resp, sb.String()
}

func TestSessionKey(t *testing.T) {
	for in, want := range map[string]string{
		"/agent/sessions/2026-09-29T17-43-34-773Z_01a0ee43.jsonl":                                                "main",
		"/agent/sessions/2026-09-29T17-43-34-773Z_01a0/9017da63-d08d-43ab-b978-e86cb7afc45b/run-0/session.jsonl": "9017da63-d08d-43ab-b978-e86cb7afc45b",
		"/agent/sessions/x/9017da63-d08d-43ab-b978-e86cb7afc45b/run-2/session.jsonl":                             "9017da63-d08d-43ab-b978-e86cb7afc45b#2",
		"": "main", "/etc/passwd": "main",
	} {
		if got := SessionKey(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestToolRequestsValidated(t *testing.T) {
	b := &fakeBackend{}
	run, rec := &fakeRunner{}, &fakeRecorder{}
	c := startPi(t, b, run, rec)
	ok := map[string]any{"toolCallId": "call_00_a", "tool": "read", "req": map[string]any{"op": "read", "path": "/workspace/a"}}
	if resp, body := postJSON(t, c, "/tool/op", ok); resp.StatusCode != http.StatusConflict {
		t.Fatalf("nicht zugewiesen: %d %s", resp.StatusCode, body)
	}
	b.chat = "chat-1"
	bad := []map[string]any{
		{"toolCallId": "", "tool": "read", "req": map[string]any{"op": "read", "path": "/a"}},
		{"toolCallId": "a b", "tool": "read", "req": map[string]any{"op": "read", "path": "/a"}},
		{"toolCallId": "c", "tool": "todo", "req": map[string]any{"op": "read", "path": "/a"}},
		{"toolCallId": "c", "tool": "read", "req": map[string]any{"op": "write", "path": "/a"}},
		{"toolCallId": "c", "tool": "read", "req": map[string]any{"op": "read", "path": "relativ"}},
		{"toolCallId": "c", "tool": "bash", "req": map[string]any{"op": "bash", "command": "ls", "cwd": "/workspace"}}, // bash nur über /tool/bash
	}
	for _, r := range bad {
		if resp, body := postJSON(t, c, "/tool/op", r); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %d %s", r, resp.StatusCode, body)
		}
	}
	if resp, _ := postJSON(t, c, "/tool/bash", ok); resp.StatusCode != http.StatusBadRequest {
		t.Error("read über /tool/bash angenommen")
	}
	if len(run.reqs) != 0 || len(rec.all()) != 0 {
		t.Fatalf("abgewiesene Anfragen ausgeführt: %v %v", run.reqs, rec.all())
	}
}

func TestToolOpRecorded(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	run, rec := &fakeRunner{file: []byte("Inhalt")}, &fakeRecorder{}
	c := startPi(t, b, run, rec)
	resp, body := postJSON(t, c, "/tool/op", map[string]any{"toolCallId": "call_00_a", "tool": "read",
		"sessionFile": "/agent/sessions/m/9017da63-d08d-43ab-b978-e86cb7afc45b/run-0/session.jsonl",
		"req":         map[string]any{"op": "read", "path": "/workspace/../workspace/a.txt"}})
	var f execproto.Frame
	_ = json.Unmarshal([]byte(body), &f)
	var rr execproto.ReadResult
	_ = json.Unmarshal(f.Result, &rr)
	if resp.StatusCode != 200 || string(rr.Data) != "Inhalt" {
		t.Fatalf("read: %d %s", resp.StatusCode, body)
	}
	if run.reqs[0].Path != "/workspace/a.txt" {
		t.Fatalf("Pfad nicht bereinigt: %q", run.reqs[0].Path)
	}
	postJSON(t, c, "/tool/op", map[string]any{"toolCallId": "call_00_b", "tool": "write",
		"req": map[string]any{"op": "write", "path": "/workspace/b.txt", "data": base64.StdEncoding.EncodeToString([]byte("geheim"))}})
	recs := rec.all()
	if len(recs) != 2 {
		t.Fatalf("Einträge: %+v", recs)
	}
	sum := sha256.Sum256([]byte("Inhalt"))
	r0 := recs[0]
	if r0.ChatID != "chat-1" || r0.SlotID != "p-test" || r0.ToolCallID != "call_00_a" || r0.Tool != "read" || r0.Op != "read" ||
		r0.Session != "9017da63-d08d-43ab-b978-e86cb7afc45b" || r0.OutputSHA256 != hex.EncodeToString(sum[:]) || r0.OutputExcerpt != "Inhalt" || r0.OutputBytes != 6 {
		t.Fatalf("read-Eintrag: %+v", r0)
	}
	if strings.Contains(string(recs[1].Args), "geheim") || !strings.Contains(string(recs[1].Args), `"bytes":6`) || recs[1].Session != "main" {
		t.Fatalf("write-Argumente: %s", recs[1].Args)
	}
}

func TestToolBashStreamAndAbort(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	run, rec := &fakeRunner{chunks: []string{"eins\n", "zwei\n"}}, &fakeRecorder{}
	c := startPi(t, b, run, rec)
	bash := map[string]any{"toolCallId": "call_01", "tool": "bash", "req": map[string]any{"op": "bash", "command": "echo x", "cwd": "/workspace",
		"env": map[string]string{"PI_SESSION_ID": "s"}}}
	resp, body := postJSON(t, c, "/tool/bash", bash)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if resp.StatusCode != 200 || len(lines) != 3 || !strings.Contains(lines[0], base64.StdEncoding.EncodeToString([]byte("eins\n"))) ||
		!strings.Contains(lines[2], `"done":true`) || !strings.Contains(lines[2], `"exit":0`) {
		t.Fatalf("Strom: %d %q", resp.StatusCode, lines)
	}
	r := rec.all()[0]
	if r.OutputExcerpt != "eins\nzwei\n" || r.OutputBytes != 10 || r.ExitCode == nil || *r.ExitCode != 0 || !strings.Contains(string(r.Args), `"command":"echo x"`) {
		t.Fatalf("bash-Eintrag: %+v", r)
	}
	// Eine fremde Umgebungsvariable wird abgewiesen.
	bad := map[string]any{"toolCallId": "call_02", "tool": "bash", "req": map[string]any{"op": "bash", "command": "echo", "cwd": "/workspace",
		"env": map[string]string{"LD_PRELOAD": "/x"}}}
	if resp, _ := postJSON(t, c, "/tool/bash", bad); resp.StatusCode != 400 {
		t.Fatalf("LD_PRELOAD: %d", resp.StatusCode)
	}
	// Abbruch: Die Verbindung wird geschlossen, die Operation endet, der Eintrag nennt es.
	run.block = true
	ctx, cancel := context.WithCancel(context.Background())
	body2, _ := json.Marshal(map[string]any{"toolCallId": "call_03", "tool": "bash", "req": map[string]any{"op": "bash", "command": "sleep 99", "cwd": "/workspace"}})
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://agw/tool/bash", strings.NewReader(string(body2)))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	_, _ = br.ReadString('\n') // erstes Stück da: Befehl läuft
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	recs := rec.all()
	if len(recs) != 2 || recs[1].ToolCallID != "call_03" || !strings.Contains(recs[1].Error, "aborted") {
		t.Fatalf("Abbruch nicht protokolliert: %+v", recs)
	}
}

func TestToolUploadFromExecSandbox(t *testing.T) {
	b := &fakeBackend{chat: "chat-1", decide: "approved"}
	run, rec := &fakeRunner{file: []byte("MCP")}, &fakeRecorder{}
	c := startPi(t, b, run, rec)
	_, body := postJSON(t, c, "/tool/upload", map[string]any{"toolCallId": "call_u", "tool": "mcp_upload_artifact", "path": "/workspace/notiz.txt"})
	var res UploadResult
	_ = json.Unmarshal([]byte(body), &res)
	if res.Status != "approved" || res.Name != "notiz.txt" || len(b.uploads) != 1 || b.uploads[0] != "chat-1|mcp|notiz.txt|MCP" {
		t.Fatalf("Upload: %s %v", body, b.uploads)
	}
	if b.calls2[0] != "call_u" {
		t.Fatalf("Kennung des Werkzeugaufrufs: %q", b.calls2)
	}
	if r := rec.all(); len(r) != 1 || r[0].Tool != "mcp_upload_artifact" || r[0].Op != "read" {
		t.Fatalf("Eintrag: %+v", r)
	}
	if !strings.Contains(strings.Join(b.calls, " "), "mcp:upload:approved") {
		t.Fatalf("Socket-Protokoll: %v", b.calls)
	}
	run.file = nil
	_, body = postJSON(t, c, "/tool/upload", map[string]any{"toolCallId": "call_v", "tool": "mcp_upload_artifact", "path": "/workspace/fehlt.txt"})
	if !strings.Contains(body, "no such file") {
		t.Fatalf("fehlende Datei: %s", body)
	}
	if resp, _ := postJSON(t, c, "/tool/upload", map[string]any{"toolCallId": "call_w", "tool": "read", "path": "/workspace/x"}); resp.StatusCode != 400 {
		t.Fatalf("Upload unter fremdem Werkzeug: %d", resp.StatusCode)
	}
}

func TestDigestExcerpt(t *testing.T) {
	d := newDigest(8)
	d.Write([]byte("abcdefghijklmnop"))
	if got := d.Excerpt(); !strings.HasPrefix(got, "abcd") || !strings.HasSuffix(got, "mnop") || !strings.Contains(got, "8 Bytes ausgelassen") {
		t.Fatalf("Auszug: %q", got)
	}
	d = newDigest(8)
	d.Write([]byte("abc"))
	d.Write([]byte("de"))
	if d.Excerpt() != "abcde" || d.n != 5 {
		t.Fatalf("kurz: %q", d.Excerpt())
	}
}

// K1: Binärausgabe (printf '\0', read einer PNG-Datei) landet ohne NUL im Protokoll, auch in
// Fehlermeldung und Argumenten. Postgres lehnte den Eintrag sonst ab.
func TestToolBinaryOutputRecordedWithoutNUL(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x10")
	run, rec := &fakeRunner{chunks: []string{"a\x00b\n"}, file: png}, &fakeRecorder{}
	c := startPi(t, b, run, rec)
	postJSON(t, c, "/tool/bash", map[string]any{"toolCallId": "call_nul", "tool": "bash",
		"req": map[string]any{"op": "bash", "command": "printf 'a\\0b\\n'", "cwd": "/workspace"}})
	postJSON(t, c, "/tool/op", map[string]any{"toolCallId": "call_png", "tool": "read", "req": map[string]any{"op": "read", "path": "/workspace/bild.png"}})
	recs := rec.all()
	if len(recs) != 2 {
		t.Fatalf("Einträge: %+v", recs)
	}
	for _, r := range recs {
		if strings.ContainsRune(r.OutputExcerpt, 0) || strings.ContainsRune(r.Error, 0) || strings.Contains(string(r.Args), `\u0000`) {
			t.Errorf("NUL im Eintrag: %q %q %s", r.OutputExcerpt, r.Error, r.Args)
		}
	}
	if recs[0].OutputExcerpt != "a␀b\n" || recs[0].OutputBytes != 4 || !strings.Contains(recs[1].OutputExcerpt, "IHDR") || recs[1].OutputBytes != int64(len(png)) {
		t.Fatalf("Auszüge: %q %q", recs[0].OutputExcerpt, recs[1].OutputExcerpt)
	}
	// Prüfsumme über die echten Bytes, nicht über den bereinigten Auszug.
	sum := sha256.Sum256(png)
	if recs[1].OutputSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("Prüfsumme: %s", recs[1].OutputSHA256)
	}
}

// gateRunner zählt gleichzeitig laufende Operationen und hält jede bis zur Freigabe.
type gateRunner struct {
	mu      sync.Mutex
	active  int
	maxSeen int
	release chan struct{}
}

func (g *gateRunner) Run(ctx context.Context, req execproto.Request, onData func([]byte)) (execproto.Frame, error) {
	g.mu.Lock()
	g.active++
	g.maxSeen = max(g.maxSeen, g.active)
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.active--; g.mu.Unlock() }()
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	b, _ := json.Marshal(execproto.ReadResult{Data: []byte("x"), Size: 1})
	return execproto.Frame{Done: true, Result: b}, nil
}

// N4: Viele gleichzeitige große reads eines Chats belegen den Orchestrator nicht mit mehreren GB;
// je Platz laufen höchstens zwei Operationen mit großem Inhalt zugleich, der Rest wartet.
func TestToolConcurrencyBoundedPerSlot(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	g := &gateRunner{release: make(chan struct{})}
	dir, _ := os.MkdirTemp("", "agwsockpi")
	defer os.RemoveAll(dir)
	srv, err := Listen(dir, NewPiHandler("p-test", b, 1<<20, g, &fakeRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 20 * time.Second}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{"toolCallId": fmt.Sprintf("call_%d", i), "tool": "read", "req": map[string]any{"op": "read", "path": "/workspace/a"}})
			resp, err := c.Post("http://agw/tool/op", "application/json", strings.NewReader(string(body)))
			if err == nil {
				resp.Body.Close()
			}
		}(i)
	}
	time.Sleep(500 * time.Millisecond)
	g.mu.Lock()
	seen := g.maxSeen
	g.mu.Unlock()
	close(g.release)
	wg.Wait()
	if seen > 2 || seen == 0 {
		t.Fatalf("%d gleichzeitige reads", seen)
	}
}

// H1: Den Pfad der ganzen Ausgabe bestimmt der Orchestrator aus der toolCallId; was die Bridge
// mitschickt, zählt nicht. Der Pfad kommt im letzten Rahmen zurück.
func TestToolBashSpillPathFromToolCallID(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	run, rec := &fakeRunner{chunks: []string{"x"}}, &fakeRecorder{}
	c := startPi(t, b, run, rec)
	_, body := postJSON(t, c, "/tool/bash", map[string]any{"toolCallId": "call_spill", "tool": "bash",
		"req": map[string]any{"op": "bash", "command": "echo", "cwd": "/workspace", "spill": "/tmp/pi-bash-0000000000000000.log"}})
	want := execproto.SpillPath("call_spill")
	if len(run.reqs) != 1 || run.reqs[0].Spill != want {
		t.Fatalf("Spill: %+v", run.reqs)
	}
	if !strings.Contains(body, `"fullOutputPath":"`+want+`"`) {
		t.Fatalf("Pfad nicht zurückgemeldet: %s", body)
	}
}

// echoDuplex spielt den Worker: Jede Eingabe kommt als Nachricht zurück; Ende der Eingaben beendet.
type echoDuplex struct{ fakeRunner }

func (e *echoDuplex) RunDuplex(ctx context.Context, req execproto.Request, input <-chan []byte, onData func([]byte)) (execproto.Frame, error) {
	for b := range input {
		onData(b)
	}
	code := 0
	return execproto.Frame{Done: true, Exit: &code}, nil
}

// C: workflowScript über /tool/workflow, Nachrichten in beiden Richtungen, protokolliert unter
// subagent/workflow mit dem Skript aus der Nachricht „start“.
func TestToolWorkflowDuplex(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	rec := &fakeRecorder{}
	c := startPiRunner(t, b, &echoDuplex{}, rec)
	body := `{"toolCallId":"call_wf","tool":"subagent","sessionFile":"","source":"worker-src"}` + "\n" +
		`{"m":{"type":"start","script":"return 42"}}` + "\n" + `{"m":{"type":"response","callId":1}}` + "\n"
	resp, err := c.Post("http://agw/tool/workflow", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 3 || !strings.Contains(lines[0], `"start"`) || !strings.Contains(lines[2], `"done":true`) {
		t.Fatalf("Antwort: %q", lines)
	}
	r := rec.all()
	if len(r) != 1 || r[0].Tool != "subagent" || r[0].Op != "workflow" || !strings.Contains(string(r[0].Args), "return 42") || r[0].ToolCallID != "call_wf" {
		t.Fatalf("Eintrag: %+v", r)
	}
	bad, _ := c.Post("http://agw/tool/workflow", "application/x-ndjson", strings.NewReader(`{"toolCallId":"x","tool":"bash","source":"s"}`+"\n"))
	if bad.StatusCode != 400 {
		t.Fatalf("fremdes Werkzeug: %d", bad.StatusCode)
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"agw/internal/execproto"
)

// do führt eine Anfrage direkt aus und sammelt die Rahmen.
func do(t *testing.T, ctx context.Context, req execproto.Request) (out []byte, last execproto.Frame) {
	t.Helper()
	runOp(ctx, req, func(f execproto.Frame) {
		out = append(out, f.Data...)
		if f.Done {
			last = f
		}
	})
	if !last.Done {
		t.Fatalf("kein abschließender Rahmen für %s", req.Op)
	}
	return out, last
}

func TestValidate(t *testing.T) {
	bad := []execproto.Request{
		{Op: "read", Path: "relativ.txt"},
		{Op: "read", Path: "/a\x00b"},
		{Op: "read", Path: "/a\nb"},
		{Op: "read", Path: ""},
		{Op: "bash", Command: "echo", Cwd: "/tmp", Env: map[string]string{"LD_PRELOAD": "/x.so"}},
		{Op: "bash", Command: "", Cwd: "/tmp"},
		{Op: "bash", Command: "echo", Cwd: "tmp"},
		{Op: "bash", Command: "echo", Cwd: "/tmp", Timeout: -1},
		{Op: "grep", Path: "/tmp"},
		{Op: "glob", Path: "/tmp", Glob: &execproto.GlobArgs{}},
		{Op: "access", Path: "/tmp", Mode: "x"},
		{Op: "rm", Path: "/tmp"},
		{Op: "write", Path: "/tmp/x", Data: make([]byte, execproto.MaxFileBytes+1)},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("angenommen: %+v", r.Op)
		}
	}
	r := execproto.Request{Op: "read", Path: "/workspace/../etc//passwd"}
	if err := r.Validate(); err != nil || r.Path != "/etc/passwd" || r.Max != execproto.MaxFileBytes {
		t.Fatalf("bereinigt: %+v %v", r, err)
	}
	g := execproto.Request{Op: "grep", Path: "/", Grep: &execproto.GrepArgs{Pattern: "x", Limit: 99999, Context: -3}}
	if err := g.Validate(); err != nil || g.Grep.Limit != execproto.MaxGrepLimit || g.Grep.Context != 0 {
		t.Fatalf("grep: %+v %v", g.Grep, err)
	}
}

func TestFileOps(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	p := filepath.Join(dir, "a", "b.txt")
	if _, f := do(t, ctx, execproto.Request{Op: "mkdir", Path: filepath.Dir(p)}); f.Error != "" {
		t.Fatal(f.Error)
	}
	if _, f := do(t, ctx, execproto.Request{Op: "write", Path: p, Data: []byte("hallo\nwelt\n")}); f.Error != "" {
		t.Fatal(f.Error)
	}
	_, f := do(t, ctx, execproto.Request{Op: "read", Path: p})
	var rr execproto.ReadResult
	_ = json.Unmarshal(f.Result, &rr)
	if string(rr.Data) != "hallo\nwelt\n" || rr.Size != 11 {
		t.Fatalf("read: %q %+v", rr.Data, f)
	}
	_, f = do(t, ctx, execproto.Request{Op: "read", Path: p, Max: 3})
	if f.Code != "EFBIG" {
		t.Fatalf("Grenze: %+v", f)
	}
	_, f = do(t, ctx, execproto.Request{Op: "read", Path: filepath.Join(dir, "fehlt")})
	if f.Code != "ENOENT" {
		t.Fatalf("fehlt: %+v", f)
	}
	_, f = do(t, ctx, execproto.Request{Op: "read", Path: dir})
	if f.Code != "EISDIR" {
		t.Fatalf("Verzeichnis: %+v", f)
	}
	// Ein FIFO darf weder Lesen noch Schreiben festhalten.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan execproto.Frame, 2)
	go func() { _, f := do(t, ctx, execproto.Request{Op: "read", Path: fifo}); done <- f }()
	go func() { _, f := do(t, ctx, execproto.Request{Op: "write", Path: fifo, Data: []byte("x")}); done <- f }()
	for i := 0; i < 2; i++ {
		select {
		case f := <-done:
			if f.Error == "" {
				t.Fatalf("FIFO angenommen: %+v", f)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("FIFO hält die Operation fest")
		}
	}
	_, f = do(t, ctx, execproto.Request{Op: "stat", Path: p})
	var st execproto.StatResult
	_ = json.Unmarshal(f.Result, &st)
	if !st.Exists || !st.IsFile || st.IsDir || st.Size != 11 || st.MtimeMs == 0 {
		t.Fatalf("stat: %+v", st)
	}
	_, f = do(t, ctx, execproto.Request{Op: "stat", Path: filepath.Join(dir, "fehlt")})
	st = execproto.StatResult{}
	_ = json.Unmarshal(f.Result, &st)
	if st.Exists {
		t.Fatal("stat eines fehlenden Pfads")
	}
	_ = os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link"))
	_, f = do(t, ctx, execproto.Request{Op: "readdir", Path: dir})
	var rd execproto.ReaddirResult
	_ = json.Unmarshal(f.Result, &rd)
	kinds := map[string]bool{}
	for _, e := range rd.Entries {
		kinds[e.Name] = e.IsDir
	}
	if !kinds["a"] || !kinds["link"] || kinds["fifo"] {
		t.Fatalf("readdir: %+v", rd.Entries)
	}
	if _, f = do(t, ctx, execproto.Request{Op: "access", Path: p, Mode: "rw"}); f.Error != "" {
		t.Fatal(f.Error)
	}
	if _, f = do(t, ctx, execproto.Request{Op: "access", Path: filepath.Join(dir, "fehlt"), Mode: "r"}); f.Code != "ENOENT" {
		t.Fatalf("access: %+v", f)
	}
	png := filepath.Join(dir, "bild.png")
	_ = os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nrest"), 0o644)
	_, f = do(t, ctx, execproto.Request{Op: "image_type", Path: png})
	var it execproto.ImageTypeResult
	_ = json.Unmarshal(f.Result, &it)
	if it.Mime != "image/png" {
		t.Fatalf("Bildtyp: %+v", it)
	}
}

func TestSniffImage(t *testing.T) {
	for in, want := range map[string]string{
		"\xff\xd8\xff\xe0": "image/jpeg", "GIF89a..": "image/gif", "RIFF\x00\x00\x00\x00WEBPVP8": "image/webp",
		"<svg": "", "": "",
	} {
		if got := sniffImage([]byte(in)); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func bashAvailable(t *testing.T) {
	if _, err := os.Stat(bashPath); err != nil {
		t.Skip("bash fehlt")
	}
}

func TestBash(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	ctx := context.Background()
	out, f := do(t, ctx, execproto.Request{Op: "bash", Command: "echo out; echo err >&2; pwd; echo $PI_SESSION_ID; exit 3", Cwd: dir,
		Env: map[string]string{"PI_SESSION_ID": "s-1"}})
	if f.Exit == nil || *f.Exit != 3 {
		t.Fatalf("Exit: %+v", f)
	}
	for _, want := range []string{"out", "err", filepath.Base(dir), "s-1"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("%q fehlt in %q", want, out)
		}
	}
	_, f = do(t, ctx, execproto.Request{Op: "bash", Command: "true", Cwd: filepath.Join(dir, "fehlt")})
	if !strings.Contains(f.Error, "Working directory does not exist") {
		t.Fatalf("cwd: %+v", f)
	}
	// Zeitgrenze
	start := time.Now()
	_, f = do(t, ctx, execproto.Request{Op: "bash", Command: "sleep 30", Cwd: dir, Timeout: 0.3})
	if f.Code != "timeout" || time.Since(start) > 5*time.Second {
		t.Fatalf("Zeitgrenze: %+v nach %v", f, time.Since(start))
	}
	// Abbruch beendet die ganze Prozessgruppe, auch Hintergrundprozesse.
	late := filepath.Join(dir, "spaet")
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start = time.Now()
	_, f = do(t, cctx, execproto.Request{Op: "bash", Command: "(sleep 1; touch " + late + ") & sleep 30", Cwd: dir})
	if f.Code != "aborted" || time.Since(start) > 5*time.Second {
		t.Fatalf("Abbruch: %+v nach %v", f, time.Since(start))
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(late); err == nil {
		t.Fatal("Hintergrundprozess lief nach dem Abbruch weiter")
	}
	// Ein Hintergrundprozess, der die Ausgabe offen hält, blockiert das Ende nicht.
	start = time.Now()
	_, f = do(t, ctx, execproto.Request{Op: "bash", Command: "sleep 5 & echo fertig", Cwd: dir})
	if f.Exit == nil || *f.Exit != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("Hintergrund: %+v nach %v", f, time.Since(start))
	}
}

func rgAvailable(t *testing.T) {
	if _, err := exec.LookPath(rgPath); err != nil {
		t.Skip("ripgrep fehlt")
	}
}

func TestGrepAndGlob(t *testing.T) {
	rgAvailable(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "src", "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "src", "a.py"), []byte("eins\nzwei Treffer\ndrei\nvier Treffer\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("kein\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "src", "node_modules", "x.py"), []byte("Treffer\n"), 0o644)
	ctx := context.Background()
	_, f := do(t, ctx, execproto.Request{Op: "grep", Path: dir, Grep: &execproto.GrepArgs{Pattern: "Treffer", Glob: "a.py", Context: 1, Limit: 1}})
	var g execproto.GrepResult
	if err := json.Unmarshal(f.Result, &g); err != nil || f.Error != "" {
		t.Fatalf("grep: %+v %v", f, err)
	}
	if !g.IsDir || !g.LimitReached || g.Matches != 1 || len(g.Lines) != 3 {
		t.Fatalf("grep: %+v", g)
	}
	if g.Lines[1].Text != "zwei Treffer" || !g.Lines[1].Match || g.Lines[0].Match || g.Lines[0].Path != "src/a.py" {
		t.Fatalf("Zeilen: %+v", g.Lines)
	}
	_, f = do(t, ctx, execproto.Request{Op: "grep", Path: filepath.Join(dir, "src", "a.py"), Grep: &execproto.GrepArgs{Pattern: "treffer", IgnoreCase: true}})
	g = execproto.GrepResult{}
	_ = json.Unmarshal(f.Result, &g)
	if g.IsDir || g.Matches != 2 || g.Lines[0].Path != "a.py" || g.Lines[0].Line != 2 {
		t.Fatalf("Datei: %+v", g)
	}
	_, f = do(t, ctx, execproto.Request{Op: "grep", Path: dir, Grep: &execproto.GrepArgs{Pattern: "(", Literal: false}})
	if f.Error == "" {
		t.Fatal("ungültiger regulärer Ausdruck angenommen")
	}
	_, f = do(t, ctx, execproto.Request{Op: "grep", Path: dir, Grep: &execproto.GrepArgs{Pattern: "gibtsnicht"}})
	g = execproto.GrepResult{}
	_ = json.Unmarshal(f.Result, &g)
	if f.Error != "" || g.Matches != 0 {
		t.Fatalf("ohne Treffer: %+v", f)
	}
}

// M2: find wie pi mit fd: Verzeichnisse dabei, Muster mit „/“ gegen den ganzen Pfad (mit
// „**/“ davor), Teilergebnis statt Fehler, wenn fd mit Ausgabe scheitert. Der Gleichlauf mit
// pis eingebautem find steht im Docker-Test (TestBridgeParity).
func TestGlobWithFd(t *testing.T) {
	if _, err := exec.LookPath(fdPath); err != nil {
		t.Skip("fd fehlt (läuft in der Ausführungs-Sandbox, siehe TestBridgeParity)")
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "src", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "src", "sub", "a.py"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.py"), []byte("x"), 0o644)
	ctx := context.Background()
	paths := func(pattern string) []string {
		_, f := do(t, ctx, execproto.Request{Op: "glob", Path: dir, Glob: &execproto.GlobArgs{Pattern: pattern}})
		var gl execproto.GlobResult
		_ = json.Unmarshal(f.Result, &gl)
		sort.Strings(gl.Paths)
		return gl.Paths
	}
	if got := paths("*.py"); len(got) != 2 {
		t.Fatalf("*.py: %v", got)
	}
	if got := paths("sub"); len(got) != 1 || got[0] != filepath.Join(dir, "src", "sub")+"/" {
		t.Fatalf("Verzeichnis: %v", got)
	}
	if got := paths("sub/*.py"); len(got) != 1 {
		t.Fatalf("Muster mit /: %v", got)
	}
	_, f := do(t, ctx, execproto.Request{Op: "glob", Path: filepath.Join(dir, "fehlt"), Glob: &execproto.GlobArgs{Pattern: "*"}})
	if f.Error == "" {
		t.Fatalf("glob fehlt: %+v", f)
	}
}

// L5: Treffer in Dateien mit ungültigem UTF-8 (rg liefert dann „bytes“ statt „text“).
func TestGrepNonUTF8(t *testing.T) {
	rgAvailable(t)
	dir := t.TempDir()
	name := filepath.Join(dir, "d\xe4tei.txt")
	if err := os.WriteFile(name, []byte("vor\nNadel \xff\xfe hier\nnach\n"), 0o644); err != nil {
		t.Skip("Dateiname mit ungültigem UTF-8 nicht möglich: ", err)
	}
	for _, c := range []int{0, 1} {
		_, f := do(t, context.Background(), execproto.Request{Op: "grep", Path: dir, Grep: &execproto.GrepArgs{Pattern: "Nadel", Context: c}})
		var g execproto.GrepResult
		_ = json.Unmarshal(f.Result, &g)
		var match *execproto.GrepLine
		for i := range g.Lines {
			if g.Lines[i].Match {
				match = &g.Lines[i]
			}
		}
		if g.Matches != 1 || match == nil || !strings.HasPrefix(match.Text, "Nadel") || !strings.Contains(match.Text, "hier") ||
			!strings.HasPrefix(match.Path, "d") || match.Line != 2 || (c == 1 && len(g.Lines) != 3) {
			t.Fatalf("Kontext %d: %+v %s", c, g, f.Error)
		}
	}
}

// L3: ls überspringt Einträge, die sich nicht stat'en lassen (kaputte Symlinks), wie pi.
func TestReaddirSkipsBrokenSymlinks(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "da.txt"), nil, 0o644)
	_ = os.Symlink(filepath.Join(dir, "gibtsnicht"), filepath.Join(dir, "kaputt"))
	_, f := do(t, context.Background(), execproto.Request{Op: "readdir", Path: dir})
	var rd execproto.ReaddirResult
	_ = json.Unmarshal(f.Result, &rd)
	if len(rd.Entries) != 1 || rd.Entries[0].Name != "da.txt" {
		t.Fatalf("readdir: %+v", rd.Entries)
	}
}

// H1: Die ganze Ausgabe eines Befehls landet in /tmp/pi-bash-<…>.log der Ausführungs-Sandbox,
// wenn sie über pis Schwellen liegt (50 KiB oder 2000 Zeilen), sonst gibt es keine Datei.
func TestBashSpill(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	ctx := context.Background()
	spillFile := execproto.SpillPath(fmt.Sprintf("test-%d", time.Now().UnixNano()))
	t.Cleanup(func() { os.Remove(spillFile) })
	run := func(cmd string) execproto.Frame {
		_, f := do(t, ctx, execproto.Request{Op: "bash", Command: cmd, Cwd: dir, Spill: spillFile})
		if f.Error != "" {
			t.Fatalf("%s: %+v", cmd, f)
		}
		return f
	}
	if f := run("printf 'kurz\\n'"); f.FullOutputPath != "" {
		t.Fatalf("kurze Ausgabe mit Datei: %+v", f)
	}
	if _, err := os.Stat(spillFile); err == nil {
		t.Fatal("Datei bleibt bei kurzer Ausgabe liegen")
	}
	f := run("head -c 60000 /dev/zero | tr '\\0' a; echo; exit 4")
	if f.FullOutputPath != spillFile || *f.Exit != 4 {
		t.Fatalf("lange Ausgabe: %+v", f)
	}
	if b, _ := os.ReadFile(spillFile); len(b) != 60001 {
		t.Fatalf("Datei: %d Bytes", len(b))
	}
	if f := run("seq 1 2001"); f.FullOutputPath != spillFile {
		t.Fatalf("viele Zeilen: %+v", f)
	}
	if f := run("seq 1 2000"); f.FullOutputPath != "" {
		t.Fatalf("2000 Zeilen sind noch keine Kürzung: %+v", f)
	}
	// Ungültiges UTF-8 wird beim Dekodieren länger (U+FFFD): 20 000 Bytes 0xff zählen wie 60 000.
	if f := run("head -c 20000 /dev/zero | LC_ALL=C tr '\\0' '\\377'"); f.FullOutputPath != spillFile {
		t.Fatalf("Binärausgabe: %+v", f)
	}
	// Obergrenze mit Vermerk
	old := maxSpillForTest(1000)
	defer maxSpillForTest(old)
	if f := run("head -c 70000 /dev/zero | tr '\\0' b"); f.FullOutputPath != spillFile {
		t.Fatalf("über der Grenze: %+v", f)
	}
	b, _ := os.ReadFile(spillFile)
	if !strings.HasPrefix(string(b), strings.Repeat("b", 1000)) || !strings.Contains(string(b), "[output truncated after") || len(b) > 1200 {
		t.Fatalf("gekürzte Datei: %d Bytes, Ende %q", len(b), b[len(b)-60:])
	}
}

// read über MaxFileBytes: Ausschnitt zeilenweise, Zeilenzahl wie split("\n").
func TestReadLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gross.txt")
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, "Zeile %d\n", i)
	}
	sb.WriteString(strings.Repeat("x", 100000) + "\nletzte")
	_ = os.WriteFile(p, []byte(sb.String()), 0o644)
	get := func(start int64, count, maxBytes int) execproto.LinesResult {
		_, f := do(t, context.Background(), execproto.Request{Op: "read_lines", Path: p, Lines: &execproto.LinesArgs{Start: start, Count: count, MaxBytes: maxBytes}})
		var r execproto.LinesResult
		if err := json.Unmarshal(f.Result, &r); err != nil || f.Error != "" {
			t.Fatalf("read_lines: %v %+v", err, f)
		}
		return r
	}
	r := get(0, 2001, 51200)
	if r.TotalLines != 5002 || len(r.Lines) != 2001 || string(r.Lines[0]) != "Zeile 1" || r.StartLineBytes != 7 {
		t.Fatalf("Anfang: %d Zeilen, gesamt %d, %q", len(r.Lines), r.TotalLines, r.Lines[0])
	}
	r = get(5000, 2001, 51200)
	if len(r.Lines) != 1 || r.StartLineBytes != 100000 || len(r.Lines[0]) != 51201 {
		t.Fatalf("lange Zeile: %d Zeilen, Länge %d/%d", len(r.Lines), len(r.Lines[0]), r.StartLineBytes)
	}
	r = get(4990, 5, 51200)
	if len(r.Lines) != 5 || string(r.Lines[4]) != "Zeile 4995" {
		t.Fatalf("Grenze Count: %q", r.Lines)
	}
	r = get(5001, 10, 100)
	if len(r.Lines) != 1 || string(r.Lines[0]) != "letzte" {
		t.Fatalf("Ende: %q", r.Lines)
	}
}

func TestPollSubagents(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "main", "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b", "run-0")
	_ = os.MkdirAll(run, 0o755)
	_ = os.WriteFile(filepath.Join(run, "session.jsonl"), []byte("{\"a\":1}\n{\"b\":2}\n{\"unvollständig"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "subagent-artifacts"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "subagent-artifacts", "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b_scout_0_input.md"), nil, 0o644)
	var out bytes.Buffer
	if err := pollSubagents(strings.NewReader(`{"offsets":{}}`), &out, root, ""); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Files []struct {
			Path   string
			Offset int64
			Data   string
		}
		Agents map[string]string
	}
	_ = json.Unmarshal(out.Bytes(), &res)
	if len(res.Files) != 1 || res.Files[0].Data != "{\"a\":1}\n{\"b\":2}\n" || res.Files[0].Offset != 16 {
		t.Fatalf("Dateien: %+v", res.Files)
	}
	if res.Agents["0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b"] != "scout" {
		t.Fatalf("Agenten: %+v", res.Agents)
	}
	out.Reset()
	in, _ := json.Marshal(map[string]any{"offsets": map[string]int64{res.Files[0].Path: 16}})
	_ = pollSubagents(bytes.NewReader(in), &out, root, "")
	if strings.Contains(out.String(), "\"b\"") {
		t.Fatalf("schon gelesene Zeilen erneut: %s", out.String())
	}
}

func TestPut(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x", "y.json")
	if err := put(p, strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "{}" {
		t.Fatal(string(b))
	}
	if err := put("relativ", strings.NewReader("")); err == nil {
		t.Fatal("relativer Pfad angenommen")
	}
}

// --- Überwacher über einen echten Kindprozess ---

var helperBin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "agw-exec-test")
	helperBin = filepath.Join(dir, "agw-exec")
	cmd := exec.Command("go", "build", "-o", helperBin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type serveHarness struct {
	in     io.WriteCloser
	frames chan execproto.Frame
	done   chan struct{}
}

func startServe(t *testing.T) *serveHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := newServer(outW, helperBin, -1, -1)
	h := &serveHarness{in: inW, frames: make(chan execproto.Frame, 256), done: make(chan struct{})}
	go func() { s.run(inR); outW.Close(); close(h.done) }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64<<10), execproto.MaxFrameBytes)
		for sc.Scan() {
			var f execproto.Frame
			_ = json.Unmarshal(sc.Bytes(), &f)
			h.frames <- f
		}
		close(h.frames)
	}()
	t.Cleanup(func() { inW.Close(); <-h.done })
	return h
}

func (h *serveHarness) send(t *testing.T, r execproto.Request) {
	b, _ := json.Marshal(r)
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// collect wartet auf die abschließenden Rahmen der genannten IDs.
func (h *serveHarness) collect(t *testing.T, ids ...uint64) map[uint64][]execproto.Frame {
	t.Helper()
	got := map[uint64][]execproto.Frame{}
	open := map[uint64]bool{}
	for _, id := range ids {
		open[id] = true
	}
	timeout := time.After(15 * time.Second)
	for len(open) > 0 {
		select {
		case f := <-h.frames:
			got[f.ID] = append(got[f.ID], f)
			if f.Done {
				delete(open, f.ID)
			}
		case <-timeout:
			t.Fatalf("keine Antwort für %v", open)
		}
	}
	return got
}

func TestServeProtocol(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	h := startServe(t)
	h.send(t, execproto.Request{ID: 1, Op: "bash", Command: "sleep 30", Cwd: dir})
	h.send(t, execproto.Request{ID: 2, Op: "write", Path: filepath.Join(dir, "x.txt"), Data: []byte("x")})
	h.send(t, execproto.Request{ID: 3, Op: "unbekannt", Path: dir})
	h.send(t, execproto.Request{ID: 4, Op: "bash", Command: "printf 'a\\nb\\n'", Cwd: dir})
	got := h.collect(t, 2, 3, 4)
	if last := got[2][len(got[2])-1]; last.Error != "" {
		t.Fatalf("write: %+v", last)
	}
	if last := got[3][0]; last.Code != "EINVAL" {
		t.Fatalf("unbekannte Operation: %+v", last)
	}
	var out []byte
	for _, f := range got[4] {
		out = append(out, f.Data...)
	}
	if string(out) != "a\nb\n" || *got[4][len(got[4])-1].Exit != 0 {
		t.Fatalf("bash: %q", out)
	}
	// Abbruch über das Protokoll, während Nr. 1 noch läuft
	h.send(t, execproto.Request{ID: 1, Op: "cancel"})
	got = h.collect(t, 1)
	if last := got[1][len(got[1])-1]; last.Code != "aborted" {
		t.Fatalf("Abbruch: %+v", last)
	}
}

// Das Ende von stdin (Orchestrator weg) bricht laufende Befehle ab.
func TestServeStopsOnEOF(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	h := startServe(t)
	marker := filepath.Join(dir, "lief-weiter")
	h.send(t, execproto.Request{ID: 7, Op: "bash", Command: "sleep 2; touch " + marker, Cwd: dir})
	time.Sleep(300 * time.Millisecond)
	h.in.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Überwacher endet nicht")
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Befehl lief nach dem Ende des Überwachers weiter")
	}
}

// K1: NUL in Suchmustern wird abgewiesen (rg nähme es ohnehin nicht, und das Protokoll soll
// keine NUL-Bytes tragen).
func TestValidateRejectsNULInPatterns(t *testing.T) {
	for _, r := range []execproto.Request{
		{Op: "grep", Path: "/tmp", Grep: &execproto.GrepArgs{Pattern: "a\x00b"}},
		{Op: "grep", Path: "/tmp", Grep: &execproto.GrepArgs{Pattern: "a", Glob: "*\x00.py"}},
		{Op: "glob", Path: "/tmp", Glob: &execproto.GlobArgs{Pattern: "*\x00"}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("angenommen: %+v %+v", r.Grep, r.Glob)
		}
	}
}

// Hintergrundaufgabe (Operation bg): meldet zuerst ihre Prozessgruppe, streamt die Ausgabe,
// behält die Ausgabedatei auch bei kurzer Ausgabe und endet mit dem Exit-Code.
func TestBgOp(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	var frames []execproto.Frame
	runOp(context.Background(), execproto.Request{Op: execproto.OpBg, Command: "echo a; sleep 0.2; echo b; exit 3", Cwd: dir, Spill: "/tmp/agw-bg/bg-1.log"},
		func(f execproto.Frame) { frames = append(frames, f) })
	if len(frames) < 2 || frames[0].Pgid <= 1 || frames[0].Done {
		t.Fatalf("erster Rahmen ohne Prozessgruppe: %+v", frames)
	}
	var out []byte
	for _, f := range frames {
		out = append(out, f.Data...)
	}
	last := frames[len(frames)-1]
	if string(out) != "a\nb\n" || !last.Done || last.Exit == nil || *last.Exit != 3 {
		t.Fatalf("Ausgabe %q, Ende %+v", out, last)
	}
	want := filepath.Join(dir, "bg-1.log")
	if last.FullOutputPath != want {
		t.Fatalf("Ausgabedatei: %q", last.FullOutputPath)
	}
	if b, _ := os.ReadFile(want); string(b) != "a\nb\n" {
		t.Fatalf("Datei: %q", b)
	}
	// Ohne gültigen Pfad keine Hintergrundaufgabe.
	for _, p := range []string{"", "/tmp/pi-bash-0123456789abcdef.log", "/tmp/agw-bg/../x.log", "/tmp/agw-bg/bg-0.log", "/workspace/bg-1.log"} {
		r := execproto.Request{Op: execproto.OpBg, Command: "true", Cwd: dir, Spill: p}
		if err := r.Validate(); err == nil {
			t.Errorf("Pfad angenommen: %q", p)
		}
	}
	if p := execproto.BgLogPath(12); !execproto.BgLogRe.MatchString(p) || p != "/tmp/agw-bg/bg-12.log" {
		t.Fatalf("BgLogPath: %q", p)
	}
}

// Über den Überwacher: höchstens bgMax Hintergrundaufgaben zugleich, Abbruch beendet die
// Prozessgruppe, danach ist wieder Platz.
func TestServeBgLimitAndStop(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	h := startServe(t)
	marker := filepath.Join(dir, "lief-weiter")
	for i := uint64(1); i <= execproto.DefaultBgMax; i++ {
		h.send(t, execproto.Request{ID: i, Op: execproto.OpBg, Command: fmt.Sprintf("sleep 1.5; touch %s-%d; sleep 30", marker, i), Cwd: dir, Spill: execproto.BgLogPath(int(i))})
	}
	// Die ersten Rahmen (Prozessgruppen) abwarten, damit alle als laufend zählen.
	pgids := map[uint64]int{}
	deadline := time.After(10 * time.Second)
	for len(pgids) < execproto.DefaultBgMax {
		select {
		case f := <-h.frames:
			if f.Done {
				t.Fatalf("Aufgabe %d endet vorzeitig: %+v", f.ID, f)
			}
			if f.Pgid > 0 {
				pgids[f.ID] = f.Pgid
			}
		case <-deadline:
			t.Fatalf("Prozessgruppen: %v", pgids)
		}
	}
	// Eine weitere Hintergrundaufgabe wird abgewiesen, ein gewöhnlicher Befehl nicht.
	h.send(t, execproto.Request{ID: 50, Op: execproto.OpBg, Command: "true", Cwd: dir, Spill: execproto.BgLogPath(50)})
	h.send(t, execproto.Request{ID: 51, Op: execproto.OpBash, Command: "echo frei", Cwd: dir})
	got := h.collect(t, 50, 51)
	if last := got[50][len(got[50])-1]; last.Code != "ELIMIT" || !strings.Contains(last.Error, fmt.Sprint(execproto.DefaultBgMax)) {
		t.Fatalf("Grenze: %+v", last)
	}
	if last := got[51][len(got[51])-1]; last.Exit == nil || *last.Exit != 0 {
		t.Fatalf("bash neben den Hintergrundaufgaben: %+v", last)
	}
	// Abbruch von Nr. 1: Gruppe weg (die Marke entsteht nie), danach ist wieder Platz.
	h.send(t, execproto.Request{ID: 1, Op: execproto.OpCancel})
	got = h.collect(t, 1)
	if last := got[1][len(got[1])-1]; last.Code != "aborted" {
		t.Fatalf("Abbruch: %+v", last)
	}
	if syscall.Kill(-pgids[1], 0) == nil {
		t.Fatal("Prozessgruppe lebt nach dem Abbruch")
	}
	h.send(t, execproto.Request{ID: 60, Op: execproto.OpBg, Command: "echo wieder", Cwd: dir, Spill: execproto.BgLogPath(60)})
	got = h.collect(t, 60)
	if last := got[60][len(got[60])-1]; last.Exit == nil || *last.Exit != 0 {
		t.Fatalf("nach dem Abbruch: %+v", last)
	}
	time.Sleep(1600 * time.Millisecond)
	if _, err := os.Stat(marker + "-1"); err == nil {
		t.Fatal("abgebrochene Aufgabe lief weiter")
	}
	if _, err := os.Stat(marker + "-2"); err != nil {
		t.Fatal("andere Aufgabe lief nicht weiter:", err)
	}
}

// Endet der Helfer einer Hintergrundaufgabe ohne Ergebnis (hier: SIGKILL, wie ihn auch der Agent
// schicken könnte), beendet der Überwacher die Prozessgruppe der Aufgabe.
func TestServeBgHelperKilled(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	h := startServe(t)
	h.send(t, execproto.Request{ID: 9, Op: execproto.OpBg, Command: "sleep 30", Cwd: dir, Spill: execproto.BgLogPath(9)})
	var pgid int
	select {
	case f := <-h.frames:
		pgid = f.Pgid
	case <-time.After(10 * time.Second):
		t.Fatal("kein erster Rahmen")
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", fmt.Sprint(pgid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	var helper int
	fmt.Sscan(strings.TrimSpace(string(out)), &helper)
	if helper <= 1 {
		t.Fatalf("Helfer nicht gefunden: %q", out)
	}
	_ = syscall.Kill(helper, syscall.SIGKILL)
	got := h.collect(t, 9)
	if last := got[9][len(got[9])-1]; last.Code != "EIO" {
		t.Fatalf("Ende: %+v", last)
	}
	for i := 0; i < 50 && syscall.Kill(-pgid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(-pgid, 0) == nil {
		t.Fatal("Prozessgruppe lebt nach dem Ende des Helfers weiter")
	}
}

// Name und Zustand aus den Statusdateien von pi-subagents: Die Datei des Kindes gewinnt gegen seine
// Zeile im Workflow; parallele Schritte bekommen #n.
func TestSubagentRuns(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		_ = os.MkdirAll(filepath.Join(dir, name), 0o755)
		_ = os.WriteFile(filepath.Join(dir, name, "status.json"), []byte(body), 0o644)
	}
	sf := func(id string, n int) string {
		return fmt.Sprintf("/agent/sessions/2026-main/%s/run-%d/session.jsonl", id, n)
	}
	write("wf", `{"runId":"wf","mode":"workflow","state":"running","steps":[
		{"agent":"researcher","label":"reid","status":"running","sessionFile":"`+sf("3942c48e-7d41-49af-a5a1-db3cd71b8d5f", 0)+`"},
		{"agent":"researcher","label":"daten","status":"running","sessionFile":"`+sf("4bc90811-79fb-49f9-ad95-ae4fede09b33", 0)+`"}]}`)
	write("child", `{"runId":"child","mode":"single","state":"complete","workflowKey":"reid","parentWorkflowRunId":"wf","endedAt":5,
		"steps":[{"agent":"researcher","status":"complete","sessionFile":"`+sf("3942c48e-7d41-49af-a5a1-db3cd71b8d5f", 0)+`"}]}`)
	write("par", `{"runId":"par","mode":"parallel","state":"running","steps":[
		{"agent":"scout","status":"complete","sessionFile":"`+sf("1a686988-f940-4330-a6e7-77e993b18762", 0)+`"},
		{"agent":"worker","status":"running","sessionFile":"`+sf("1a686988-f940-4330-a6e7-77e993b18762", 1)+`"}]}`)
	write("kaputt", `{nicht json`)
	r := subagentRuns(filepath.Join(dir, "*", "status.json"))
	if got := r["3942c48e-7d41-49af-a5a1-db3cd71b8d5f"]; got.Label != "reid" || got.State != "complete" || got.PiRun != "child" || got.Parent != "wf" {
		t.Fatalf("Kind: %+v", got)
	}
	if got := r["4bc90811-79fb-49f9-ad95-ae4fede09b33"]; got.Label != "daten" || got.State != "running" || got.Agent != "researcher" {
		t.Fatalf("aus dem Workflow: %+v", got)
	}
	if r["1a686988-f940-4330-a6e7-77e993b18762"].State != "complete" || r["1a686988-f940-4330-a6e7-77e993b18762#1"].Agent != "worker" {
		t.Fatalf("parallel: %+v", r)
	}
}

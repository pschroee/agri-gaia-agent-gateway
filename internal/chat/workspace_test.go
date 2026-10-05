package chat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

type tarEntry struct {
	name, body, link string
	typ              byte
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, ModTime: time.Unix(1700000000, 0)}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func tarNames(t *testing.T, data []byte) map[string]string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		v := string(b)
		if h.Linkname != "" {
			v = "->" + h.Linkname
		}
		out[h.Name] = v
	}
}

// The archive comes from the sandbox and can be shaped by the agent: only
// harmless entries are restored.
func TestFilterWorkspaceArchive(t *testing.T) {
	in := makeTarGz(t, []tarEntry{
		{name: "./", typ: tar.TypeDir},
		{name: "./plot.png", typ: tar.TypeReg, body: "png"},
		{name: "./sub/", typ: tar.TypeDir},
		{name: "./sub/script.py", typ: tar.TypeReg, body: "print(1)"},
		{name: "./link", typ: tar.TypeSymlink, link: "/etc/passwd"},      // symlink stays a symlink
		{name: "./hard", typ: tar.TypeLink, link: "./plot.png"},          // hard link within the archive: ok
		{name: "./hardout", typ: tar.TypeLink, link: "/etc/passwd"},      // hard link to the outside: dropped
		{name: "../outside.txt", typ: tar.TypeReg, body: "x"},            // ..: dropped
		{name: "/abs.txt", typ: tar.TypeReg, body: "x"},                  // absolute: dropped
		{name: "./a/../../b.txt", typ: tar.TypeReg, body: "x"},           // ..: dropped
		{name: "./inputs/data.csv", typ: tar.TypeReg, body: "x"},         // comes from the inputs: dropped
		{name: "./home", typ: tar.TypeSymlink, link: "/home/agent"},      // symlink to a folder …
		{name: "./home/.bashrc", typ: tar.TypeReg, body: "evil"},         // … writing below it: dropped
		{name: "./fifo", typ: tar.TypeFifo},                              // FIFO: dropped
		{name: "./link", typ: tar.TypeReg, body: "replaces the symlink"}, // replacing the symlink: dropped
	})
	var out bytes.Buffer
	kept, dropped, err := filterWorkspaceArchive(bytes.NewReader(in), &out, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := tarNames(t, out.Bytes())
	want := map[string]string{"plot.png": "png", "sub/": "", "sub/script.py": "print(1)", "link": "->/etc/passwd", "hard": "->plot.png", "home": "->/home/agent"}
	var gk, wk []string
	for k := range got {
		gk = append(gk, k+"="+got[k])
	}
	for k := range want {
		wk = append(wk, k+"="+want[k])
	}
	sort.Strings(gk)
	sort.Strings(wk)
	if strings.Join(gk, ",") != strings.Join(wk, ",") {
		t.Fatalf("kept:\n%v\nwant:\n%v", gk, wk)
	}
	if kept != 6 || dropped != 8 {
		t.Fatalf("kept=%d dropped=%d", kept, dropped)
	}
}

func TestFilterWorkspaceArchiveLimitAndGarbage(t *testing.T) {
	in := makeTarGz(t, []tarEntry{{name: "large.bin", typ: tar.TypeReg, body: strings.Repeat("x", 100)}})
	if _, _, err := filterWorkspaceArchive(bytes.NewReader(in), io.Discard, 50); err == nil {
		t.Fatal("limit not enforced when unpacking")
	}
	if _, _, err := filterWorkspaceArchive(strings.NewReader("not gzip"), io.Discard, 50); err == nil {
		t.Fatal("no error for a broken archive")
	}
}

// The backup script excludes inputs/ and package and cache folders, in find
// (size, fingerprint) and tar alike.
func TestWorkspaceSaveScriptExcludes(t *testing.T) {
	for _, x := range []string{"-path ./inputs", "--exclude=./inputs"} {
		if !strings.Contains(workspaceSaveScript, x) {
			t.Errorf("missing: %s", x)
		}
	}
	for _, x := range []string{"node_modules", ".venv", "__pycache__", ".cache"} {
		if !strings.Contains(workspaceSaveScript, "-name "+x) || !strings.Contains(workspaceSaveScript, "--exclude="+x) {
			t.Errorf("exclusion %s missing in find or tar", x)
		}
	}
}

type stubExec struct {
	out []byte
	err error
}

func (s stubExec) Exec(context.Context, []string, io.Reader) ([]byte, error) { return s.out, s.err }

func TestSnapshotWorkspaceParsing(t *testing.T) {
	ctx := context.Background()
	s, err := snapshotWorkspace(ctx, stubExec{out: []byte("SKIP fp 3 999\n")}, 100, "")
	if err != nil || s.Status != "SKIP" || s.Files != 3 || s.Size != 999 {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = snapshotWorkspace(ctx, stubExec{out: append([]byte("DATA fp 1 3\n"), "gz"...)}, 100, "old")
	if err != nil || s.Status != "DATA" || string(s.Archive) != "gz" || s.Fingerprint != "fp" {
		t.Fatalf("%+v %v", s, err)
	}
	// More than the hard upper bound (file grew while packing): skipped.
	big := append([]byte("DATA fp 1 3\n"), bytes.Repeat([]byte("x"), int(workspaceArchiveCap(100))+1)...)
	s, err = snapshotWorkspace(ctx, stubExec{out: big, err: errors.New("exit 141")}, 100, "")
	if err != nil || s.Status != "SKIP" || s.Archive != nil {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := snapshotWorkspace(ctx, stubExec{out: []byte("nonsense")}, 100, ""); err == nil {
		t.Fatal("unexpected output not detected")
	}
}

func TestFormatMB(t *testing.T) {
	if got := formatMB(1258291); got != "1.2 MB" {
		t.Fatal(got)
	}
}

// After a run /workspace is backed up, and on resuming it is restored in the
// fresh sandbox before the first message.
func TestWorkspaceSavedAfterRunAndRestoredOnResume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "ws"})
	if err != nil {
		t.Fatal(err)
	}
	archive := makeTarGz(t, []tarEntry{
		{name: "./", typ: tar.TypeDir},
		{name: "./plot.png", typ: tar.TypeReg, body: "png"},
		{name: "./note.txt", typ: tar.TypeReg, body: "hello"},
	})
	a := e.agent(0)
	a.mu.Lock()
	a.wsOut = append([]byte("DATA fp1 2 8\n"), archive...)
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "Create files"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	var w store.Workspace
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if w, err = e.st.GetWorkspace(ctx, c.ID); err == nil {
			break
		}
	}
	if err != nil || w.Files != 2 || w.Size != 8 || w.Fingerprint != "fp1" || w.ObjectKey != c.ID+"/workspace.tar.gz" {
		t.Fatalf("not backed up: %+v %v", w, err)
	}
	e.blobs.mu.Lock()
	stored := e.blobs.m[w.ObjectKey]
	e.blobs.mu.Unlock()
	if !bytes.Equal(stored, archive) {
		t.Fatal("archive not in storage")
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.Workspace == nil || v.Workspace.Files != 2 || v.Workspace.SavedAt == nil {
		t.Fatalf("API field: %+v", v.Workspace)
	}
	// Idling: the script runs once more (fingerprint fp1 → unchanged, nothing new).
	a.mu.Lock()
	a.wsOut = []byte("SAME fp1 2 8\n")
	before := a.wsSaves
	a.mu.Unlock()
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	after := a.wsSaves
	a.mu.Unlock()
	if after != before+1 {
		t.Fatalf("not backed up when idling: %d → %d", before, after)
	}
	if _, err := e.m.Send(ctx, c.ID, "List the files"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	fresh := resumedAgent(e)
	if fresh == nil || fresh == a {
		t.Fatal("no fresh sandbox")
	}
	fresh.mu.Lock()
	restored, late := fresh.wsRestored, fresh.wsLatePrompt
	fresh.mu.Unlock()
	got := tarNames(t, restored)
	if got["plot.png"] != "png" || got["note.txt"] != "hello" || len(got) != 2 {
		t.Fatalf("restored: %v", got)
	}
	if late {
		t.Fatal("workspace only restored after the first message")
	}
}

// Above the limit nothing is backed up; the last valid backup stays, and the
// chat is told once.
func TestWorkspaceOverLimitKeepsLastBackup(t *testing.T) {
	e := setup(t)
	e.m.opt.WorkspaceMaxBytes = 100
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "large"})
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	a := e.agent(0)
	archive := makeTarGz(t, []tarEntry{{name: "a.txt", typ: tar.TypeReg, body: "a"}})
	a.mu.Lock()
	a.wsOut = append([]byte("DATA fp1 1 1\n"), archive...)
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "one"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.Fingerprint == "fp1" })

	a.mu.Lock()
	a.wsOut = []byte("SKIP fp2 5 300000000\n")
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "two"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	w := waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.SkippedReason != nil })
	if w.Fingerprint != "fp1" || w.Files != 1 || !strings.Contains(*w.SkippedReason, "286.1 MB") {
		t.Fatalf("after exceeding: %+v %s", w, *w.SkippedReason)
	}
	ev := waitEvent(t, events, "error", "")
	if msg := ev.Data.(map[string]string)["message"]; !strings.HasPrefix(msg, "Workspace not saved") || !strings.Contains(msg, "backup from") {
		t.Fatalf("notice: %q", msg)
	}
	e.blobs.mu.Lock()
	stored := e.blobs.m[c.ID+"/workspace.tar.gz"]
	e.blobs.mu.Unlock()
	if !bytes.Equal(stored, archive) {
		t.Fatal("last valid backup overwritten")
	}
	// Below the limit again, state as backed up: the note disappears.
	a.mu.Lock()
	a.wsOut = []byte("SAME fp1 1 1\n")
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "three"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.SkippedReason == nil })
}

// If restoring fails, nothing is backed up in this sandbox: an empty workspace
// must not overwrite the valid backup.
func TestWorkspaceRestoreFailureProtectsBackup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "broken"})
	a := e.agent(0)
	a.mu.Lock()
	a.wsOut = append([]byte("DATA fp1 1 1\n"), makeTarGz(t, []tarEntry{{name: "a.txt", typ: tar.TypeReg, body: "a"}})...)
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "one"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.Fingerprint == "fp1" })
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	// Storage altered: the checksum no longer matches.
	e.blobs.mu.Lock()
	e.blobs.m[c.ID+"/workspace.tar.gz"] = []byte("altered")
	e.blobs.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "two"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	time.Sleep(100 * time.Millisecond) // background work after agent_settled
	fresh := resumedAgent(e)
	if fresh == nil || fresh == a {
		t.Fatal("no fresh sandbox")
	}
	fresh.mu.Lock()
	restored, saves := fresh.wsRestored, fresh.wsSaves
	fresh.mu.Unlock()
	if restored != nil {
		t.Error("altered archive restored")
	}
	if saves != 0 {
		t.Errorf("backed up %d times after a failed restore", saves)
	}
	w, _ := e.st.GetWorkspace(ctx, c.ID)
	if w.Fingerprint != "fp1" {
		t.Fatalf("backup changed: %+v", w)
	}
}

func waitWorkspace(t *testing.T, e *env, chatID string, ok func(store.Workspace) bool) store.Workspace {
	t.Helper()
	var w store.Workspace
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var err error
		if w, err = e.st.GetWorkspace(context.Background(), chatID); err == nil && ok(w) {
			return w
		}
	}
	t.Fatalf("workspace not in the expected state: %+v", w)
	return w
}

// resumedAgent is the sandbox in which a chat was resumed (switch_session).
func resumedAgent(e *env) *fakeAgent {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, x := range e.agents {
		x.mu.Lock()
		for _, c := range x.cmds {
			if c["type"] == "switch_session" {
				x.mu.Unlock()
				return x
			}
		}
		x.mu.Unlock()
	}
	return nil
}

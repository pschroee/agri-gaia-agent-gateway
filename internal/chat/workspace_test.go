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

// Das Archiv stammt aus der Sandbox und ist vom Agenten gestaltbar: Nur
// unbedenkliche Einträge werden eingespielt.
func TestFilterWorkspaceArchive(t *testing.T) {
	in := makeTarGz(t, []tarEntry{
		{name: "./", typ: tar.TypeDir},
		{name: "./plot.png", typ: tar.TypeReg, body: "png"},
		{name: "./sub/", typ: tar.TypeDir},
		{name: "./sub/skript.py", typ: tar.TypeReg, body: "print(1)"},
		{name: "./link", typ: tar.TypeSymlink, link: "/etc/passwd"},     // Symlink bleibt Symlink
		{name: "./hard", typ: tar.TypeLink, link: "./plot.png"},         // harter Link im Archiv: ok
		{name: "./hardaus", typ: tar.TypeLink, link: "/etc/passwd"},     // harter Link nach außen: weg
		{name: "../aussen.txt", typ: tar.TypeReg, body: "x"},            // ..: weg
		{name: "/abs.txt", typ: tar.TypeReg, body: "x"},                 // absolut: weg
		{name: "./a/../../b.txt", typ: tar.TypeReg, body: "x"},          // ..: weg
		{name: "./inputs/daten.csv", typ: tar.TypeReg, body: "x"},       // kommt aus den Eingaben: weg
		{name: "./home", typ: tar.TypeSymlink, link: "/home/agent"},     // Symlink auf Ordner …
		{name: "./home/.bashrc", typ: tar.TypeReg, body: "boese"},       // … darunter schreiben: weg
		{name: "./fifo", typ: tar.TypeFifo},                             // FIFO: weg
		{name: "./link", typ: tar.TypeReg, body: "ersetzt den Symlink"}, // Symlink ersetzen: weg
	})
	var out bytes.Buffer
	kept, dropped, err := filterWorkspaceArchive(bytes.NewReader(in), &out, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := tarNames(t, out.Bytes())
	want := map[string]string{"plot.png": "png", "sub/": "", "sub/skript.py": "print(1)", "link": "->/etc/passwd", "hard": "->plot.png", "home": "->/home/agent"}
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
		t.Fatalf("übernommen:\n%v\nerwartet:\n%v", gk, wk)
	}
	if kept != 6 || dropped != 8 {
		t.Fatalf("kept=%d dropped=%d", kept, dropped)
	}
}

func TestFilterWorkspaceArchiveLimitAndGarbage(t *testing.T) {
	in := makeTarGz(t, []tarEntry{{name: "gross.bin", typ: tar.TypeReg, body: strings.Repeat("x", 100)}})
	if _, _, err := filterWorkspaceArchive(bytes.NewReader(in), io.Discard, 50); err == nil {
		t.Fatal("Grenze beim Entpacken nicht durchgesetzt")
	}
	if _, _, err := filterWorkspaceArchive(strings.NewReader("kein gzip"), io.Discard, 50); err == nil {
		t.Fatal("kein Fehler bei kaputtem Archiv")
	}
}

// Das Sicherungsskript schließt inputs/ und Paket- und Cache-Ordner aus, in find
// (Größe, Fingerabdruck) und tar gleichermaßen.
func TestWorkspaceSaveScriptExcludes(t *testing.T) {
	for _, x := range []string{"-path ./inputs", "--exclude=./inputs"} {
		if !strings.Contains(workspaceSaveScript, x) {
			t.Errorf("fehlt: %s", x)
		}
	}
	for _, x := range []string{"node_modules", ".venv", "__pycache__", ".cache"} {
		if !strings.Contains(workspaceSaveScript, "-name "+x) || !strings.Contains(workspaceSaveScript, "--exclude="+x) {
			t.Errorf("Ausschluss %s fehlt in find oder tar", x)
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
	s, err = snapshotWorkspace(ctx, stubExec{out: append([]byte("DATA fp 1 3\n"), "gz"...)}, 100, "alt")
	if err != nil || s.Status != "DATA" || string(s.Archive) != "gz" || s.Fingerprint != "fp" {
		t.Fatalf("%+v %v", s, err)
	}
	// Mehr als die harte Obergrenze (Datei während des Packens gewachsen): ausgelassen.
	big := append([]byte("DATA fp 1 3\n"), bytes.Repeat([]byte("x"), int(workspaceArchiveCap(100))+1)...)
	s, err = snapshotWorkspace(ctx, stubExec{out: big, err: errors.New("exit 141")}, 100, "")
	if err != nil || s.Status != "SKIP" || s.Archive != nil {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := snapshotWorkspace(ctx, stubExec{out: []byte("Unsinn")}, 100, ""); err == nil {
		t.Fatal("unerwartete Ausgabe nicht erkannt")
	}
}

func TestFormatMB(t *testing.T) {
	if got := formatMB(1258291); got != "1,2 MB" {
		t.Fatal(got)
	}
}

// Nach einem Lauf wird /workspace gesichert, beim Fortsetzen in der frischen
// Sandbox vor dem ersten Auftrag eingespielt.
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
		{name: "./notiz.txt", typ: tar.TypeReg, body: "hallo"},
	})
	a := e.agent(0)
	a.mu.Lock()
	a.wsOut = append([]byte("DATA fp1 2 8\n"), archive...)
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "Erzeuge Dateien"); err != nil {
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
		t.Fatalf("nicht gesichert: %+v %v", w, err)
	}
	e.blobs.mu.Lock()
	stored := e.blobs.m[w.ObjectKey]
	e.blobs.mu.Unlock()
	if !bytes.Equal(stored, archive) {
		t.Fatal("Archiv nicht in der Ablage")
	}
	v, _ := e.m.View(ctx, c.ID)
	if v.Workspace == nil || v.Workspace.Files != 2 || v.Workspace.SavedAt == nil {
		t.Fatalf("API-Feld: %+v", v.Workspace)
	}
	// Ruhen: Das Skript läuft noch einmal (Fingerabdruck fp1 → unverändert, nichts Neues).
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
		t.Fatalf("beim Ruhen nicht gesichert: %d → %d", before, after)
	}
	if _, err := e.m.Send(ctx, c.ID, "Liste die Dateien"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	fresh := resumedAgent(e)
	if fresh == nil || fresh == a {
		t.Fatal("keine frische Sandbox")
	}
	fresh.mu.Lock()
	restored, late := fresh.wsRestored, fresh.wsLatePrompt
	fresh.mu.Unlock()
	got := tarNames(t, restored)
	if got["plot.png"] != "png" || got["notiz.txt"] != "hallo" || len(got) != 2 {
		t.Fatalf("eingespielt: %v", got)
	}
	if late {
		t.Fatal("Arbeitsbereich erst nach dem ersten Auftrag eingespielt")
	}
}

// Über der Grenze wird nicht gesichert; die letzte gültige Sicherung bleibt, und
// der Chat erfährt es einmal.
func TestWorkspaceOverLimitKeepsLastBackup(t *testing.T) {
	e := setup(t)
	e.m.opt.WorkspaceMaxBytes = 100
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "gross"})
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	a := e.agent(0)
	archive := makeTarGz(t, []tarEntry{{name: "a.txt", typ: tar.TypeReg, body: "a"}})
	a.mu.Lock()
	a.wsOut = append([]byte("DATA fp1 1 1\n"), archive...)
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "eins"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.Fingerprint == "fp1" })

	a.mu.Lock()
	a.wsOut = []byte("SKIP fp2 5 300000000\n")
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "zwei"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	w := waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.SkippedReason != nil })
	if w.Fingerprint != "fp1" || w.Files != 1 || !strings.Contains(*w.SkippedReason, "286,1 MB") {
		t.Fatalf("nach Überschreitung: %+v %s", w, *w.SkippedReason)
	}
	ev := waitEvent(t, events, "error", "")
	if msg := ev.Data.(map[string]string)["message"]; !strings.HasPrefix(msg, "Arbeitsbereich nicht gesichert") || !strings.Contains(msg, "Sicherung von") {
		t.Fatalf("Hinweis: %q", msg)
	}
	e.blobs.mu.Lock()
	stored := e.blobs.m[c.ID+"/workspace.tar.gz"]
	e.blobs.mu.Unlock()
	if !bytes.Equal(stored, archive) {
		t.Fatal("letzte gültige Sicherung überschrieben")
	}
	// Wieder unter der Grenze, Stand wie gesichert: Vermerk verschwindet.
	a.mu.Lock()
	a.wsOut = []byte("SAME fp1 1 1\n")
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "drei"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.SkippedReason == nil })
}

// Scheitert das Einspielen, wird in dieser Sandbox nicht gesichert: Ein leerer
// Arbeitsbereich darf die gültige Sicherung nicht überschreiben.
func TestWorkspaceRestoreFailureProtectsBackup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Title: "kaputt"})
	a := e.agent(0)
	a.mu.Lock()
	a.wsOut = append([]byte("DATA fp1 1 1\n"), makeTarGz(t, []tarEntry{{name: "a.txt", typ: tar.TypeReg, body: "a"}})...)
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "eins"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	waitWorkspace(t, e, c.ID, func(w store.Workspace) bool { return w.Fingerprint == "fp1" })
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	// Ablage verändert: Prüfsumme stimmt nicht mehr.
	e.blobs.mu.Lock()
	e.blobs.m[c.ID+"/workspace.tar.gz"] = []byte("verändert")
	e.blobs.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "zwei"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	time.Sleep(100 * time.Millisecond) // Hintergrundarbeit nach agent_settled
	fresh := resumedAgent(e)
	if fresh == nil || fresh == a {
		t.Fatal("keine frische Sandbox")
	}
	fresh.mu.Lock()
	restored, saves := fresh.wsRestored, fresh.wsSaves
	fresh.mu.Unlock()
	if restored != nil {
		t.Error("verändertes Archiv eingespielt")
	}
	if saves != 0 {
		t.Errorf("nach gescheitertem Einspielen %d-mal gesichert", saves)
	}
	w, _ := e.st.GetWorkspace(ctx, c.ID)
	if w.Fingerprint != "fp1" {
		t.Fatalf("Sicherung verändert: %+v", w)
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
	t.Fatalf("Arbeitsbereich nicht im erwarteten Zustand: %+v", w)
	return w
}

// resumedAgent ist die Sandbox, in der ein Chat fortgesetzt wurde (switch_session).
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

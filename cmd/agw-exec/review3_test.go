package main

// Tests zu Review 3: Eine FIFO an der Adresse der Ausgabedatei hält den Helfer nicht fest (N1),
// der Überwacher legt Verzeichnis und Datei der Hintergrundaufgaben selbst an (N1), und nach
// execproto.BgThrottleAfter liest der Helfer nur noch gedrosselt (N3).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agw/internal/execproto"
)

// runOpTimeout führt runOp aus und bricht den Test ab, wenn die Operation hängt.
func runOpTimeout(t *testing.T, req execproto.Request, d time.Duration) []execproto.Frame {
	t.Helper()
	var frames []execproto.Frame
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		runOp(ctx, req, func(f execproto.Frame) { frames = append(frames, f) })
		close(done)
	}()
	select {
	case <-done:
		return frames
	case <-time.After(d):
		cancel()
		t.Fatalf("Operation hängt (%s %q)", req.Op, req.Spill)
		return nil
	}
}

// N1: Eine FIFO an der Adresse der Ausgabedatei (vom Agenten vorab angelegt) hält weder eine
// Hintergrundaufgabe noch bash fest; der Befehl läuft ohne Datei, der Fehler steht im Ergebnis.
func TestSpillFifoDoesNotBlock(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	fifo := filepath.Join(dir, "bg-1.log")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	frames := runOpTimeout(t, execproto.Request{Op: execproto.OpBg, Command: "echo a", Cwd: dir, Spill: "/tmp/agw-bg/bg-1.log"}, 5*time.Second)
	last := frames[len(frames)-1]
	if frames[0].Pgid <= 1 || last.Exit == nil || *last.Exit != 0 || last.SpillError == "" || last.FullOutputPath != "" {
		t.Fatalf("Hintergrundaufgabe an einer FIFO: %+v", frames)
	}
	// bash: gleiche Prüfung für /tmp/pi-bash-*.log (Pfad unter dem Testverzeichnis nachgebildet).
	old := spillDirForTest(dir)
	defer spillDirForTest(old)
	sp := execproto.SpillPath("call_fifo")
	if err := syscall.Mkfifo(filepath.Join(dir, filepath.Base(sp)), 0o644); err != nil {
		t.Fatal(err)
	}
	frames = runOpTimeout(t, execproto.Request{Op: execproto.OpBash, Command: "seq 1 5000", Cwd: dir, Spill: sp}, 5*time.Second)
	if last := frames[len(frames)-1]; last.Exit == nil || *last.Exit != 0 || last.SpillError == "" {
		t.Fatalf("bash an einer FIFO: %+v", last)
	}
	// Ein Verweis wird nicht verfolgt.
	target := filepath.Join(dir, "ziel")
	_ = os.WriteFile(target, []byte("unverändert"), 0o644)
	_ = os.Remove(fifo)
	_ = os.Symlink(target, fifo)
	runOpTimeout(t, execproto.Request{Op: execproto.OpBg, Command: "echo b", Cwd: dir, Spill: "/tmp/agw-bg/bg-1.log"}, 5*time.Second)
	if b, _ := os.ReadFile(target); string(b) != "unverändert" {
		t.Fatalf("über einen Verweis geschrieben: %q", b)
	}
}

// N1: Der Überwacher legt das Verzeichnis selbst an (ein vorhandenes fremdes, etwa ein Verweis,
// wird beiseitegeräumt) und öffnet die Datei für den Helfer; eine FIFO darin ersetzt er.
func TestServeCreatesBgLog(t *testing.T) {
	bashAvailable(t)
	base := t.TempDir()
	dir := filepath.Join(base, "agw-bg")
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	elsewhere := filepath.Join(base, "anderswo")
	_ = os.Mkdir(elsewhere, 0o777)
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	h := startServe(t)
	h.send(t, execproto.Request{ID: 1, Op: execproto.OpBg, Command: "echo eins", Cwd: base, Spill: execproto.BgLogPath(1)})
	got := h.collect(t, 1)
	if last := got[1][len(got[1])-1]; last.Exit == nil || *last.Exit != 0 || last.FullOutputPath != filepath.Join(dir, "bg-1.log") || last.SpillError != "" {
		t.Fatalf("Ende: %+v", last)
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Fatalf("Verzeichnis: %v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bg-1.log")); string(b) != "eins\n" {
		t.Fatalf("Datei: %q", b)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatalf("über den Verweis geschrieben: %v", ents)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "bg-2.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.send(t, execproto.Request{ID: 2, Op: execproto.OpBg, Command: "echo zwei", Cwd: base, Spill: execproto.BgLogPath(2)})
	got = h.collect(t, 2)
	if last := got[2][len(got[2])-1]; last.Exit == nil || last.SpillError != "" {
		t.Fatalf("mit FIFO: %+v", last)
	}
	if st, _ := os.Lstat(filepath.Join(dir, "bg-2.log")); st == nil || !st.Mode().IsRegular() {
		t.Fatalf("FIFO nicht ersetzt: %v", st)
	}
}

// N3: Nach bgThrottleAfter liest der Helfer nur noch mit bgThrottleRate (der Befehl wartet beim
// Schreiben); darunter läuft die Ausgabe ungebremst.
func TestBgThrottle(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	oldAfter, oldRate := bgThrottleAfter, bgThrottleRate
	defer func() { bgThrottleAfter, bgThrottleRate = oldAfter, oldRate }()
	run := func(seq int) (time.Duration, int) {
		start := time.Now()
		frames := runOpTimeout(t, execproto.Request{Op: execproto.OpBg, Command: "head -c 3145728 /dev/zero", Cwd: dir, Spill: execproto.BgLogPath(seq)}, 20*time.Second)
		n := 0
		for _, f := range frames {
			n += len(f.Data)
		}
		return time.Since(start), n
	}
	fast, n := run(1)
	if n != 3<<20 {
		t.Fatalf("Ausgabe: %d", n)
	}
	bgThrottleAfter, bgThrottleRate = 1<<20, 4<<20
	slow, n := run(2)
	t.Logf("3 MiB ungebremst %v, ab 1 MiB mit 4 MiB/s %v", fast.Round(time.Millisecond), slow.Round(time.Millisecond))
	if n != 3<<20 || slow < 400*time.Millisecond || fast > 400*time.Millisecond {
		t.Fatalf("Drosselung: ungebremst %v, gedrosselt %v (%d Bytes)", fast, slow, n)
	}
	if !strings.HasPrefix(filepath.Base(execproto.BgLogPath(2)), "bg-") {
		t.Fatal("Pfad")
	}
}

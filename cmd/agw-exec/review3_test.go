package main

// Tests for Review 3: a FIFO at the path of the output file does not hold up the helper (N1),
// the supervisor creates the directory and file of background tasks itself (N1), and after
// execproto.BgThrottleAfter the helper only reads throttled (N3).

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

// runOpTimeout runs runOp and fails the test if the operation hangs.
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
		t.Fatalf("operation hangs (%s %q)", req.Op, req.Spill)
		return nil
	}
}

// N1: a FIFO at the path of the output file (created beforehand by the agent) holds up neither a
// background task nor bash; the command runs without a file, the error is in the result.
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
		t.Fatalf("background task at a FIFO: %+v", frames)
	}
	// bash: same check for /tmp/pi-bash-*.log (path recreated under the test directory).
	old := spillDirForTest(dir)
	defer spillDirForTest(old)
	sp := execproto.SpillPath("call_fifo")
	if err := syscall.Mkfifo(filepath.Join(dir, filepath.Base(sp)), 0o644); err != nil {
		t.Fatal(err)
	}
	frames = runOpTimeout(t, execproto.Request{Op: execproto.OpBash, Command: "seq 1 5000", Cwd: dir, Spill: sp}, 5*time.Second)
	if last := frames[len(frames)-1]; last.Exit == nil || *last.Exit != 0 || last.SpillError == "" {
		t.Fatalf("bash at a FIFO: %+v", last)
	}
	// A symlink is not followed.
	target := filepath.Join(dir, "target")
	_ = os.WriteFile(target, []byte("unchanged"), 0o644)
	_ = os.Remove(fifo)
	_ = os.Symlink(target, fifo)
	runOpTimeout(t, execproto.Request{Op: execproto.OpBg, Command: "echo b", Cwd: dir, Spill: "/tmp/agw-bg/bg-1.log"}, 5*time.Second)
	if b, _ := os.ReadFile(target); string(b) != "unchanged" {
		t.Fatalf("written through a symlink: %q", b)
	}
}

// N1: the supervisor creates the directory itself (an existing foreign one, e.g. a symlink,
// is moved aside) and opens the file for the helper; it replaces a FIFO in it.
func TestServeCreatesBgLog(t *testing.T) {
	bashAvailable(t)
	base := t.TempDir()
	dir := filepath.Join(base, "agw-bg")
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	elsewhere := filepath.Join(base, "elsewhere")
	_ = os.Mkdir(elsewhere, 0o777)
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	h := startServe(t)
	h.send(t, execproto.Request{ID: 1, Op: execproto.OpBg, Command: "echo one", Cwd: base, Spill: execproto.BgLogPath(1)})
	got := h.collect(t, 1)
	if last := got[1][len(got[1])-1]; last.Exit == nil || *last.Exit != 0 || last.FullOutputPath != filepath.Join(dir, "bg-1.log") || last.SpillError != "" {
		t.Fatalf("end: %+v", last)
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Fatalf("directory: %v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bg-1.log")); string(b) != "one\n" {
		t.Fatalf("file: %q", b)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatalf("written through the symlink: %v", ents)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "bg-2.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.send(t, execproto.Request{ID: 2, Op: execproto.OpBg, Command: "echo two", Cwd: base, Spill: execproto.BgLogPath(2)})
	got = h.collect(t, 2)
	if last := got[2][len(got[2])-1]; last.Exit == nil || last.SpillError != "" {
		t.Fatalf("with FIFO: %+v", last)
	}
	if st, _ := os.Lstat(filepath.Join(dir, "bg-2.log")); st == nil || !st.Mode().IsRegular() {
		t.Fatalf("FIFO not replaced: %v", st)
	}
}

// N3: after bgThrottleAfter the helper only reads at bgThrottleRate (the command waits when
// writing); below that the output runs unthrottled.
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
		t.Fatalf("output: %d", n)
	}
	bgThrottleAfter, bgThrottleRate = 1<<20, 4<<20
	slow, n := run(2)
	t.Logf("3 MiB unthrottled %v, from 1 MiB at 4 MiB/s %v", fast.Round(time.Millisecond), slow.Round(time.Millisecond))
	if n != 3<<20 || slow < 400*time.Millisecond || fast > 400*time.Millisecond {
		t.Fatalf("throttling: unthrottled %v, throttled %v (%d bytes)", fast, slow, n)
	}
	if !strings.HasPrefix(filepath.Base(execproto.BgLogPath(2)), "bg-") {
		t.Fatal("path")
	}
}

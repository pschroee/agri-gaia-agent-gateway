package main

// Tests for the fixed upper limit of the child process (opLimit, BM17): a background task
// outlives it, a file operation or search still ends at it.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"agw/internal/execproto"
)

// runChild runs one request through serveOp with a short opLimit and returns its frames.
// stdin stays open until the operation has ended (its end would abort the operation).
func runChild(t *testing.T, limit time.Duration, req execproto.Request) []execproto.Frame {
	t.Helper()
	old := opLimit
	opLimit = limit
	t.Cleanup(func() { opLimit = old })
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	t.Cleanup(func() { inW.Close() })
	go func() {
		b, _ := json.Marshal(req)
		_, _ = inW.Write(append(b, '\n'))
	}()
	ret := make(chan int, 1)
	go func() { ret <- serveOp(inR, outW); outW.Close() }()
	var frames []execproto.Frame
	got := make(chan struct{})
	go func() {
		defer close(got)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64<<10), execproto.MaxFrameBytes)
		for sc.Scan() {
			var f execproto.Frame
			_ = json.Unmarshal(sc.Bytes(), &f)
			frames = append(frames, f)
		}
	}()
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("the operation does not end")
	}
	if code := <-ret; code != 0 {
		t.Fatalf("serveOp returned %d", code)
	}
	if len(frames) == 0 || !frames[len(frames)-1].Done {
		t.Fatalf("no final frame: %+v", frames)
	}
	return frames
}

// A background task runs past the limit of file operations and ends with its exit code.
func TestOpLimitSparesBackgroundTask(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	start := time.Now()
	frames := runChild(t, 300*time.Millisecond, execproto.Request{Op: execproto.OpBg, Command: "sleep 1.2; echo done", Cwd: dir, Spill: execproto.BgLogPath(1)})
	if d := time.Since(start); d < time.Second {
		t.Fatalf("ended after %s, before the command", d)
	}
	var out []byte
	for _, f := range frames {
		out = append(out, f.Data...)
	}
	last := frames[len(frames)-1]
	if last.Error != "" || last.Code != "" || last.Exit == nil || *last.Exit != 0 || string(out) != "done\n" {
		t.Fatalf("output %q, end %+v", out, last)
	}
	for _, op := range []string{execproto.OpBash, execproto.OpBg, execproto.OpWorkflow} {
		if opLimited(op) {
			t.Errorf("%s is limited", op)
		}
	}
}

// A search that hangs (ripgrep on a FIFO without a writer) still ends at the limit.
func TestOpLimitEndsFileOperation(t *testing.T) {
	rgAvailable(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "hangs")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("no FIFO:", err)
	}
	start := time.Now()
	frames := runChild(t, 300*time.Millisecond, execproto.Request{Op: execproto.OpGrep, Path: fifo, Grep: &execproto.GrepArgs{Pattern: "x", Limit: 10}})
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("ended only after %s", d)
	}
	// Its own code, not "aborted"; the text the agent sees stays the same.
	if last := frames[len(frames)-1]; last.Code != execproto.CodeOpLimit || last.Error != "Operation aborted" {
		t.Fatalf("end %+v", last)
	}
	for _, op := range []string{execproto.OpRead, execproto.OpWrite, execproto.OpGrep, execproto.OpGlob, execproto.OpReadLines} {
		if !opLimited(op) {
			t.Errorf("%s is not limited", op)
		}
	}
}

// Should a command ever end at the limit, it reports CodeOpLimit, not "aborted"; an abort by the
// orchestrator stays "aborted".
func TestBashEndCodeAtLimit(t *testing.T) {
	bashAvailable(t)
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	for _, c := range []struct {
		cause error
		want  string
	}{{errOpLimit, execproto.CodeOpLimit}, {nil, "aborted"}} {
		ctx, cancel := context.WithCancelCause(context.Background())
		time.AfterFunc(200*time.Millisecond, func() { cancel(c.cause) })
		_, last := do(t, ctx, execproto.Request{Op: execproto.OpBg, Command: "sleep 30", Cwd: dir, Spill: execproto.BgLogPath(1)})
		if last.Code != c.want || last.Exit != nil {
			t.Errorf("cause %v: end %+v", c.cause, last)
		}
		cancel(nil)
	}
}

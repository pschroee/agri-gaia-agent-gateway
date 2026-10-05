package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/execbox"
	"agw/internal/execproto"
)

// Background tasks against Docker: the supervisor's operation bg in the hardened
// execution sandbox. Start, output file, end, limit, abort including process group, and the
// supervisor cleans up the group when the agent kills the helper of its task. Label
// agwpoc.managed=test (testLabels), so that a restart of the orchestrator cleans up nothing.
func TestExecSandboxBackground(t *testing.T) {
	rt, ctx, net := dockerTest(t)
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-bg-" + net[len(net)-6:], Image: image, Labels: testLabels(), NoAttach: true,
		InternalNet: net, MemoryMB: 512, CPUs: 1, Pids: 128, Tmpfs: ExecTmpfs, CapAdd: ExecCaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)
	box := execbox.New(func(dctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
		in, out, closeFn, _, err := rt.ExecStream(context.WithoutCancel(dctx), inst.ID, []string{"/usr/local/bin/agw-exec", "serve", "-bg-max", "2"}, "0:0")
		return in, out, closeFn, err
	})
	defer box.Close()
	bash := func(cmd string) string {
		var b strings.Builder
		_, _ = box.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: cmd, Cwd: "/workspace"}, func(d []byte) { b.Write(d) })
		return b.String()
	}
	type run struct {
		pgid  int
		out   strings.Builder
		frame execproto.Frame
		err   error
		done  chan struct{}
		start chan struct{}
	}
	var mu sync.Mutex
	startBg := func(c context.Context, seq int, cmd string) *run {
		r := &run{done: make(chan struct{}), start: make(chan struct{})}
		go func() {
			defer close(r.done)
			r.frame, r.err = box.RunBackground(c, execproto.Request{Op: execproto.OpBg, Command: cmd, Cwd: "/workspace", Spill: execproto.BgLogPath(seq)},
				func(pg int) { r.pgid = pg; close(r.start) }, func(d []byte) { mu.Lock(); r.out.Write(d); mu.Unlock() })
		}()
		return r
	}
	waitStart := func(r *run) {
		t.Helper()
		select {
		case <-r.start:
		case <-r.done:
			t.Fatalf("not started: %+v %v", r.frame, r.err)
		case <-time.After(10 * time.Second):
			t.Fatal("start takes too long")
		}
	}
	waitDone := func(r *run) {
		t.Helper()
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
			t.Fatal("end does not come")
		}
	}

	// Review 3, N1: the agent creates a FIFO in advance at the address of a future output file (and
	// of an output file of bash). The directory belongs to the supervisor (root), so that fails
	// there; at bash's address the FIFO does not block the helper.
	fifo := bash("mkdir -p /tmp/agw-bg; mkfifo /tmp/agw-bg/bg-6.log 2>&1 && echo CREATED; mkfifo " + execproto.SpillPath("call_fifo") + " && echo BASH-FIFO")
	t.Logf("FIFO attempt: %q", fifo)
	if strings.Contains(fifo, "CREATED") || !strings.Contains(fifo, "BASH-FIFO") {
		t.Fatalf("FIFO in the background task directory: %q", fifo)
	}
	fctx, fcancel := context.WithTimeout(ctx, 10*time.Second)
	var fout strings.Builder
	ff, ferr := box.Run(fctx, execproto.Request{Op: execproto.OpBash, Command: "seq 1 20000", Cwd: "/workspace", Spill: execproto.SpillPath("call_fifo")}, func(d []byte) { fout.Write(d) })
	fcancel()
	if ferr != nil || ff.Exit == nil || *ff.Exit != 0 || ff.SpillError == "" || !strings.HasSuffix(fout.String(), "20000\n") {
		t.Fatalf("bash with FIFO at the output file: %+v %v", ff, ferr)
	}

	// End with exit code; output streamed and in /tmp/agw-bg/bg-1.log. Directory and file are created
	// by the supervisor (root); the agent reads them but cannot modify them.
	r1 := startBg(ctx, 1, "echo start; sleep 1; echo finish-bg; exit 7")
	waitStart(r1)
	waitDone(r1)
	if r1.err != nil || r1.frame.Exit == nil || *r1.frame.Exit != 7 || r1.frame.FullOutputPath != "/tmp/agw-bg/bg-1.log" {
		t.Fatalf("end: %+v %v", r1.frame, r1.err)
	}
	if r1.out.String() != "start\nfinish-bg\n" {
		t.Fatalf("output: %q", r1.out.String())
	}
	if out := bash("stat -c '%u %a' /tmp/agw-bg /tmp/agw-bg/bg-1.log; cat /tmp/agw-bg/bg-1.log; (echo x >> /tmp/agw-bg/bg-1.log) 2>/dev/null || echo READONLY; touch /tmp/agw-bg/new 2>/dev/null || echo NO-CREATE"); out != "0 755\n0 644\nstart\nfinish-bg\nREADONLY\nNO-CREATE\n" {
		t.Fatalf("file: %q", out)
	}

	// Limit 2: two run, the third is refused; bash keeps working.
	c2, cancel2 := context.WithCancel(ctx)
	r2 := startBg(c2, 2, "sleep 300 & sleep 301; echo never")
	r3 := startBg(ctx, 3, "sleep 302")
	waitStart(r2)
	waitStart(r3)
	r4 := startBg(ctx, 4, "true")
	waitDone(r4)
	if r4.frame.Code != "ELIMIT" || !strings.Contains(r4.frame.Error, "limit 2") {
		t.Fatalf("limit: %+v", r4.frame)
	}
	if out := bash("echo free"); out != "free\n" {
		t.Fatalf("bash next to two background tasks: %q", out)
	}

	// Abort (bg_stop): the whole process group is gone afterwards, including the background process.
	stopAt := time.Now()
	cancel2()
	waitDone(r2)
	if r2.frame.Code != "aborted" {
		t.Fatalf("abort: %+v %v", r2.frame, r2.err)
	}
	t.Logf("abort after %d ms", time.Since(stopAt).Milliseconds())
	if out := bash("pgrep -f 'sleep 30[01]' || echo none"); out != "none\n" {
		t.Fatalf("processes after the abort: %q", out)
	}

	// The agent kills the helper of its task (same user, SIGKILL): the supervisor
	// (root with CAP_KILL) cleans up the group, the operation ends with an error.
	out := bash(`for d in /proc/[0-9]*; do [ "$(tr '\0' ' ' < $d/cmdline 2>/dev/null)" = "sleep 302 " ] || continue; p=${d#/proc/}; while [ "$p" -gt 1 ] && [ "$(tr '\0' ' ' < /proc/$p/cmdline)" != "/usr/local/bin/agw-exec op " ]; do p=$(awk '/^PPid/{print $2}' /proc/$p/status); done; echo "helper=$(tr '\0' ' ' < /proc/$p/cmdline)"; kill -9 $p && echo KILLED; done`)
	if !strings.Contains(out, "helper=/usr/local/bin/agw-exec op") || !strings.Contains(out, "KILLED") {
		t.Fatalf("helper not found: %q", out)
	}
	waitDone(r3)
	if r3.frame.Code != "EIO" {
		t.Fatalf("end without helper: %+v %v", r3.frame, r3.err)
	}
	time.Sleep(200 * time.Millisecond)
	if out := bash("pgrep -f 'sleep 30[2]' || echo none"); out != "none\n" {
		t.Fatalf("group lives on without helper: %q", out)
	}

	// Afterwards there is room again; the file has not grown beyond 256 MiB (sample: large).
	r5 := startBg(ctx, 5, "head -c 3000000 /dev/zero | tr '\\0' y; echo; echo done")
	waitStart(r5)
	waitDone(r5)
	if r5.frame.Exit == nil || *r5.frame.Exit != 0 {
		t.Fatalf("after the limit: %+v %v", r5.frame, r5.err)
	}
	if out := bash("stat -c %s /tmp/agw-bg/bg-5.log; tail -1 /tmp/agw-bg/bg-5.log"); out != "3000006\ndone\n" {
		t.Fatalf("large output: %q", out)
	}
	// The task whose output file the agent wanted to create in advance as a FIFO runs normally.
	rf := startBg(ctx, 6, "echo six")
	waitStart(rf)
	waitDone(rf)
	if rf.frame.Exit == nil || *rf.frame.Exit != 0 || rf.frame.SpillError != "" {
		t.Fatalf("bg-6: %+v %v", rf.frame, rf.err)
	}
	// The emergency brake (N3) still applies: after a fork bomb a background task starts again.
	bctx, bcancel := context.WithTimeout(ctx, 20*time.Second)
	_, _ = box.Run(bctx, execproto.Request{Op: execproto.OpBash, Command: "b(){ b|b & }; b", Cwd: "/workspace", Timeout: 3}, nil)
	bcancel()
	// As in TestExecSandboxForkBomb: shortly after the bomb a start can still fail with EAGAIN;
	// within 20 s it must work again.
	var r6 *run
	for i, deadline := 7, time.Now().Add(20*time.Second); ; i++ {
		r6 = startBg(ctx, i, "echo after-the-bomb")
		waitDone(r6)
		if r6.frame.Exit != nil && *r6.frame.Exit == 0 && strings.Contains(r6.out.String(), "after-the-bomb") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the fork bomb: %+v %v %q", r6.frame, r6.err, r6.out.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := json.Marshal(r6.frame)
	t.Logf("last task: %s", b)
}

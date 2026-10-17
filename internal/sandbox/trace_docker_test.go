// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sandbox

// X6 against Docker: failed syscalls of an operation appear in its final frame, and the
// agent cannot read the trace directory. Runs only with AGW_DOCKER_TESTS=1 (change to the
// execution image and the sandbox).

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/execbox"
	"agw/internal/execproto"
)

// traceSandbox starts an execution sandbox with tracing enabled and returns a client for its
// supervisor plus the container ID.
func traceSandbox(t *testing.T) (*execbox.Client, string) {
	t.Helper()
	rt, ctx, net := dockerTest(t)
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-trace-" + net[len(net)-6:], Image: image, NoAttach: true, Labels: testLabels(),
		InternalNet: net, Tmpfs: ExecTmpfs, CapAdd: ExecCaps,
		MemoryMB: 512, CPUs: 1, Pids: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), inst.ID) })
	in, out, closeFn, stderr, err := rt.ExecStream(ctx, inst.ID,
		[]string{"/usr/bin/env", "AGW_EXEC_TRACE=failed", "/usr/local/bin/agw-exec", "serve"}, "0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeFn)
	closed := false
	client := execbox.New(func(context.Context) (io.WriteCloser, io.Reader, func(), error) {
		if closed {
			return nil, nil, nil, io.ErrClosedPipe
		}
		closed = true
		return in, out, func() {}, nil
	})
	if f, err := client.Run(ctx, execproto.Request{Op: execproto.OpStat, Path: "/workspace"}, nil); err != nil || f.Error != "" {
		t.Fatalf("supervisor: %v %q (stderr: %s)", err, f.Error, stderr.String())
	}
	return client, inst.ID
}

// A connection attempt to a closed port appears as connect … ECONNREFUSED.
func TestTraceRecordsFailedConnect(t *testing.T) {
	client, _ := traceSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, err := client.Run(ctx, execproto.Request{Op: execproto.OpBash, Cwd: "/workspace",
		Command: `python3 -c 'import socket; s=socket.socket(); s.connect(("127.0.0.1",1))'`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Trace), "connect(") || !strings.Contains(string(f.Trace), "ECONNREFUSED") {
		t.Fatalf("trace without refused connection: %q", f.Trace)
	}
}

// The trace directory belongs to root and stays closed to the agent.
func TestTraceFileNotReadableByAgent(t *testing.T) {
	client, _ := traceSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []byte
	f, err := client.Run(ctx, execproto.Request{Op: execproto.OpBash, Cwd: "/workspace",
		Command: `ls /var/log/agw-trace 2>&1`}, func(b []byte) { out = append(out, b...) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Permission denied") {
		t.Fatalf("agent can list the trace directory: %q %+v", out, f)
	}
}

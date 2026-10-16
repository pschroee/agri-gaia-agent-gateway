// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// RunOnce runs one command in a fresh, sealed container and removes it again (R0.10). It is the way the
// gateway runs a check (internal/assess): the command gets its input on stdin and answers on stdout;
// stderr is kept short for the error message. Unlike a slot this container belongs to no chat, is
// attached for the run and is gone afterwards, whatever happens. The time limit comes from ctx: a
// container that outlives it is killed by the removal.
//
// maxOut bounds the answer; a longer one is an error and not read to the end (the rest of the stream is
// dropped with the container). The exit code and the tail of stderr are returned with a nil error as
// well, so that a caller can show what the command said.
//
// **The command has to stop reading stdin at the end of its input, not at EOF.** One hijacked connection
// carries both directions, and half-closing it (which is how a writer signals EOF) ends the whole stream:
// everything the container writes after that is lost. That cost the live test of R0 an empty answer with
// an exit code of 0 — the container was still running when the stream ended, and its answer never
// arrived. A tar ends with its end-of-archive marker and the check script stops there, so nothing is read
// to EOF.
func (r *Runtime) RunOnce(ctx context.Context, s Spec, stdin io.Reader, maxOut int64) ([]byte, int, string, error) {
	if maxOut <= 0 {
		maxOut = 1 << 20
	}
	ins, err := r.Start(ctx, s)
	if err != nil {
		return nil, -1, "", err
	}
	// The container always goes, also on an error and after a time limit (Force kills a running one).
	defer func() { _ = r.Remove(context.WithoutCancel(ctx), ins.ID) }()

	// stdin is written in the background and stays open until the answer has arrived (see above): the
	// command may answer before it has read everything, but the write side must not be closed early.
	go func() { _, _ = io.Copy(ins.Stdin, stdin) }()

	type read struct {
		out []byte
		err error
	}
	ch := make(chan read, 1)
	go func() {
		var buf bytes.Buffer
		// One byte more than allowed shows whether the limit was passed.
		n, err := io.Copy(&buf, io.LimitReader(ins.Stdout, maxOut+1))
		if err == nil && n > maxOut {
			err = fmt.Errorf("check output larger than %d bytes", maxOut)
		}
		ch <- read{buf.Bytes(), err}
	}()

	var got read
	select {
	case got = <-ch:
	case <-ctx.Done():
		_ = r.Remove(context.WithoutCancel(ctx), ins.ID)
		return nil, -1, ins.Stderr.String(), ctx.Err()
	}
	// The output stream ended, so the command is done with its answer; the write side may be closed now
	// (best effort: it signals an EOF the command no longer waits for).
	_ = ins.Stdin.Close()
	if got.err != nil {
		return nil, -1, ins.Stderr.String(), got.err
	}
	exit, err := r.exitCode(ctx, ins.ID)
	if err != nil {
		return got.out, -1, ins.Stderr.String(), err
	}
	return got.out, exit, ins.Stderr.String(), nil
}

// exitCode waits a moment for the end of the container whose output stream has ended and reads its exit
// code. A container that is still running has not answered completely: that is an error and not an exit
// code of 0 (exactly that masked the lost answer in the live test of R0).
func (r *Runtime) exitCode(ctx context.Context, id string) (int, error) {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	wr := r.cli.ContainerWait(waitCtx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wr.Result:
		return int(res.StatusCode), nil
	case <-wr.Error:
	case <-waitCtx.Done():
	}
	in, err := r.cli.ContainerInspect(context.WithoutCancel(ctx), id, client.ContainerInspectOptions{})
	if err != nil {
		return -1, fmt.Errorf("inspecting the container: %w", err)
	}
	if in.Container.State == nil {
		return -1, errors.New("the container has no state")
	}
	if in.Container.State.Running {
		return -1, errors.New("the container is still running although its output ended")
	}
	return in.Container.State.ExitCode, nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"

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

	// stdin is written in the background: the container may answer before it has read everything.
	go func() {
		_, _ = io.Copy(ins.Stdin, stdin)
		_ = ins.Stdin.Close()
	}()

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
	if got.err != nil {
		return nil, -1, ins.Stderr.String(), got.err
	}
	// The output stream ended, so the container has stopped; its exit code stands in its state.
	in, err := r.cli.ContainerInspect(context.WithoutCancel(ctx), ins.ID, client.ContainerInspectOptions{})
	if err != nil {
		return got.out, -1, ins.Stderr.String(), fmt.Errorf("inspecting the container: %w", err)
	}
	exit := 0
	if in.Container.State != nil {
		exit = in.Container.State.ExitCode
	}
	return got.out, exit, ins.Stderr.String(), nil
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"agw/internal/execproto"
)

// Paths in the execution sandbox (images/agw-basis/Dockerfile, target exec).
var (
	nodePath       = "/usr/local/bin/node"
	workflowRunner = "/opt/agw/workflow/runner.cjs"
)

// runWorkflow runs the script of a pi-subagents workflow (workflowScript) in its own
// Node process as the agent user, instead of in a worker thread of the pi process
// (the author's decision on P4b). req.Data is the source of the pi-subagents worker;
// it comes from the pi process, not from the agent. The agent only supplies the
// script that pi-subagents sends to the worker in the "start" message.
//
// Protocol with runner.cjs: first line {"source": …}, then one host message per line
// {"m": …}; back one line each {"m": …} (worker message) or {"__agw":"error", …}.
// Every line of the runner goes to the orchestrator as a data frame. What arrives there is output
// of the agent's code and is checked in the pi process before pi-subagents sees it.
func runWorkflow(ctx context.Context, req execproto.Request, input <-chan []byte, emit func(execproto.Frame)) {
	cmd := exec.Command(nodePath, workflowRunner)
	cmd.Dir = "/"
	cmd.Env = commandEnv(nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		emit(fail(err))
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit(fail(err))
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{b: &stderr, max: 16 << 10}
	if err := cmd.Start(); err != nil {
		emit(fail(err))
		return
	}
	pgid := cmd.Process.Pid
	first, _ := json.Marshal(map[string]string{"source": string(req.Data)})
	_, _ = stdin.Write(append(first, '\n'))
	go func() {
		for b := range input {
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				return
			}
		}
		// End of input: the host no longer needs the worker.
		_ = stdin.Close()
	}()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), execproto.MaxFrameBytes)
		for sc.Scan() {
			emit(execproto.Frame{Data: append([]byte(nil), sc.Bytes()...)})
		}
	}()
	waitDone := make(chan error, 1)
	go func() { <-readDone; waitDone <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-waitDone:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-waitDone
		emit(execproto.Frame{Done: true, Error: "aborted", Code: "aborted"})
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // stragglers of the script
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	} else if werr != nil {
		emit(fail(werr))
		return
	}
	f := execproto.Frame{Done: true, Exit: &code}
	if code != 0 {
		f.Error = strings.TrimSpace("workflow runner exited with code " + strconv.Itoa(code) + ": " + tailString(stderr.String(), 2000))
	}
	emit(f)
}

func tailString(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

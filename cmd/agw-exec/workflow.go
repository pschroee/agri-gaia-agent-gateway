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

// Pfade in der Ausführungs-Sandbox (images/agw-basis/Dockerfile, Ziel exec).
var (
	nodePath       = "/usr/local/bin/node"
	workflowRunner = "/opt/agw/workflow/runner.cjs"
)

// runWorkflow führt das Skript eines Workflows von pi-subagents (workflowScript) in einem
// eigenen Node-Prozess als Agent-Nutzer aus, statt in einem Worker-Thread des pi-Prozesses
// (Entscheidung des Verfassers zu P4b). req.Data ist der Quelltext des Workers von
// pi-subagents; er kommt aus dem pi-Prozess, nicht vom Agenten. Der Agent liefert nur das
// Skript, das pi-subagents dem Worker in der Nachricht „start“ schickt.
//
// Protokoll mit runner.cjs: erste Zeile {"source": …}, danach je Zeile eine Nachricht des
// Hosts {"m": …}; zurück je Zeile {"m": …} (Nachricht des Workers) oder {"__agw":"error", …}.
// Jede Zeile des Runners geht als Datenrahmen zum Orchestrator. Was dort ankommt, ist Ausgabe
// von Code des Agenten und wird im pi-Prozess geprüft, bevor pi-subagents es sieht.
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
		// Ende der Eingaben: Der Host braucht den Worker nicht mehr.
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
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // Nachzügler des Skripts
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

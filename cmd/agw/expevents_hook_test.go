// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

//go:build unix

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHookHelperProcess is the hook program of the hook tests: the test binary starts itself with
// GO_WANT_HELPER_PROCESS=1 (the os/exec test pattern). Mode and file come after "--".
func TestHookHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	mode, file := args[0], args[1]
	switch mode {
	case "record":
		in, _ := io.ReadAll(os.Stdin)
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(9)
		}
		fmt.Fprintf(f, "%s\t%s", os.Getenv("AGW_EXP_EVENT"), in)
		f.Close()
	case "fail":
		fmt.Fprintln(os.Stderr, "the hook is broken")
		os.Exit(3)
	case "hang":
		_ = os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// helperHook is the argv that starts TestHookHelperProcess in the given mode.
func helperHook(t *testing.T, mode, file string) []string {
	t.Helper()
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	return []string{os.Args[0], "-test.run=^TestHookHelperProcess$", "--", mode, file}
}

// #280 E3: the hook gets every event on stdin, the same line as the event file, and its name in AGW_EXP_EVENT.
func TestHookReceivesEventOnStdin(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	api := newFakeExpAPI()
	events := &bytes.Buffer{}
	var out, errw lockedBuf
	opts := expRunOptions{PlanPath: writeRunnerMaterial(t), Poll: time.Millisecond, StartGrace: 20 * time.Millisecond, Heartbeat: time.Hour,
		Events: events, Hook: helperHook(t, "record", record), HookTimeout: 20 * time.Second}
	if err := runExpPlan(context.Background(), api, opts, &out, &errw); err != nil {
		t.Fatalf("%v: %s", err, errw.String())
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.Split(strings.TrimRight(events.String(), "\n"), "\n") {
		if i := strings.Index(line, `"event":"`); i >= 0 {
			name := line[i+len(`"event":"`):]
			want = append(want, name[:strings.Index(name, `"`)]+"\t"+line)
		}
	}
	if got := strings.Split(strings.TrimRight(string(raw), "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the hook got:\n%s\nthe event file has:\n%s", raw, strings.Join(want, "\n"))
	}
}

// #280 E4: a hook that exits with an error is logged as a text line; the plan runs through.
func TestHookFailureDoesNotStopRun(t *testing.T) {
	api := newFakeExpAPI()
	var out, errw lockedBuf
	opts := expRunOptions{PlanPath: writeRunnerMaterial(t), Poll: time.Millisecond, StartGrace: 20 * time.Millisecond, Heartbeat: time.Hour,
		Hook: helperHook(t, "fail", ""), HookTimeout: 20 * time.Second}
	if err := runExpPlan(context.Background(), api, opts, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if len(api.ends) != 8 {
		t.Fatalf("%d runs ended, want 8", len(api.ends))
	}
	log := errw.String()
	if !strings.Contains(log, "hook: plan_start: exit status 3: the hook is broken") {
		t.Fatalf("no hook failure line: %s", log)
	}
}

// #280 E4: a hook that hangs is killed with its process group after the time limit and does not hold the plan.
func TestHookTimeoutKillsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	api := newFakeExpAPI()
	var out, errw lockedBuf
	opts := expRunOptions{PlanPath: writeRunnerMaterial(t), Poll: time.Millisecond, StartGrace: 20 * time.Millisecond, Heartbeat: time.Hour,
		Hook: helperHook(t, "hang", pidFile), HookTimeout: 200 * time.Millisecond}
	began := time.Now()
	if err := runExpPlan(context.Background(), api, opts, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("the hook held the plan for %s", took)
	}
	if len(api.ends) != 8 {
		t.Fatalf("%d runs ended, want 8", len(api.ends))
	}
	if !strings.Contains(errw.String(), "timed out after 200ms, killed") {
		t.Fatalf("no timeout line: %s", errw.String())
	}
	pid := 0
	for i := 0; i < 40 && pid <= 0; i++ {
		raw, _ := os.ReadFile(pidFile)
		if pid, _ = strconv.Atoi(string(raw)); pid <= 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if pid <= 0 {
		t.Fatal("the hook never wrote its process id")
	}
	gone := false
	for i := 0; i < 40 && !gone; i++ {
		gone = syscall.Kill(pid, 0) == syscall.ESRCH
		if !gone {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !gone {
		t.Fatalf("the hook process %d is still alive", pid)
	}
}

// #280 E4: a full queue drops the oldest progress event first; other events stay.
func TestHookQueueDropsOldestProgress(t *testing.T) {
	h := &expHook{wake: make(chan struct{}, 1), done: make(chan struct{})} // no worker: the queue only fills
	h.enqueue(evPlanStart, []byte("plan_start"))
	for i := 0; i < hookQueueSize-1; i++ {
		h.enqueue(evProgress, []byte(fmt.Sprintf("p%d", i)))
	}
	if len(h.queue) != hookQueueSize {
		t.Fatalf("queue %d, want %d", len(h.queue), hookQueueSize)
	}
	h.enqueue(evPaused, []byte("paused"))
	h.enqueue(evAbort, []byte("abort"))
	if len(h.queue) != hookQueueSize {
		t.Fatalf("queue grew to %d", len(h.queue))
	}
	if h.queue[0].event != evPlanStart || string(h.queue[1].line) != "p2" {
		t.Fatalf("the oldest progress events must go first: %s, %s", h.queue[0].line, h.queue[1].line)
	}
	n := len(h.queue)
	if h.queue[n-2].event != evPaused || h.queue[n-1].event != evAbort {
		t.Fatalf("the newest events must stay: %v", h.queue[n-2:])
	}
}

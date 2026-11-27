// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/agwclient"
)

// lockedBuf is a text sink that the runner's goroutine and the hook worker may write to at the same time.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newEventRunnerAt builds a runner over the material in dir with an event buffer. mutate changes the options.
func newEventRunnerAt(t *testing.T, api *fakeExpAPI, dir string, mutate func(*expRunOptions)) (*expRunner, *bytes.Buffer, *lockedBuf) {
	t.Helper()
	events := &bytes.Buffer{}
	out, errw := &lockedBuf{}, &lockedBuf{}
	opts := expRunOptions{PlanPath: dir, Poll: time.Millisecond, StartGrace: 20 * time.Millisecond, Heartbeat: time.Hour, Events: events}
	if mutate != nil {
		mutate(&opts)
	}
	r, err := newExpRunner(api, opts, out, errw)
	if err != nil {
		t.Fatal(err)
	}
	return r, events, errw
}

func newEventRunner(t *testing.T, api *fakeExpAPI, mutate func(*expRunOptions)) (*expRunner, *bytes.Buffer, *lockedBuf) {
	t.Helper()
	return newEventRunnerAt(t, api, writeRunnerMaterial(t), mutate)
}

func decodeEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event line is not JSON: %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func eventNames(evs []map[string]any) []string {
	names := make([]string, len(evs))
	for i, ev := range evs {
		names[i], _ = ev["event"].(string)
	}
	return names
}

func eventsOf(evs []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, ev := range evs {
		if ev["event"] == name {
			out = append(out, ev)
		}
	}
	return out
}

func num(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("not a number: %#v", v)
	}
	return f
}

func sub(t *testing.T, ev map[string]any, field string) map[string]any {
	t.Helper()
	m, ok := ev[field].(map[string]any)
	if !ok {
		t.Fatalf("event %v has no object %q: %v", ev["event"], field, ev)
	}
	return m
}

var eventTimeRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

// #280 E6: a plan without --cell gives plan_start, per run run_pending/run_start/run_end/progress, a cell_start before the
// first and a cell_end after the last run of every cell, and plan_end. seq has no gaps, ts is UTC with Z.
func TestEventsJSONLOrderAndFields(t *testing.T) {
	api := newFakeExpAPI()
	r, events, _ := newEventRunner(t, api, nil)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := decodeEvents(t, events)
	names := eventNames(evs)
	if names[0] != "plan_start" || names[len(names)-1] != "plan_end" {
		t.Fatalf("sequence: %v", names)
	}
	session := evs[0]["session"]
	for i, ev := range evs {
		if num(t, ev["seq"]) != float64(i+1) {
			t.Fatalf("event %d has seq %v", i, ev["seq"])
		}
		if num(t, ev["v"]) != 1 || ev["session"] != session || ev["plan"] != r.planName || ev["material"] != shortID(r.planSHA) {
			t.Fatalf("common fields of event %d: %v", i, ev)
		}
		if ts, _ := ev["ts"].(string); !eventTimeRe.MatchString(ts) {
			t.Fatalf("ts %q is not UTC with milliseconds and Z", ts)
		}
	}
	for name, want := range map[string]int{"run_pending": 8, "run_start": 8, "run_end": 8, "progress": 8, "cell_start": 2, "cell_end": 2, "plan_start": 1, "plan_end": 1} {
		if got := len(eventsOf(evs, name)); got != want {
			t.Errorf("%d %s events, want %d (%v)", got, name, want, names)
		}
	}
	start := evs[0]
	if num(t, start["cells"]) != 2 || num(t, start["k"]) != 2 || num(t, start["seed"]) != 7 || num(t, start["runs"]) != 8 ||
		start["lock"] != false || num(t, start["ended_before"]) != 0 {
		t.Fatalf("plan_start: %v", start)
	}
	// Every run_end closes the run_start before it and carries the same key and attempt.
	open := ""
	for _, ev := range evs {
		switch ev["event"] {
		case "run_start":
			if open != "" {
				t.Fatalf("run_start of %v inside the run %s", ev["key"], open)
			}
			open, _ = ev["key"].(string)
		case "run_end":
			if ev["key"] != open || ev["attempt"] != open {
				t.Fatalf("run_end %v does not close %s", ev, open)
			}
			open = ""
		}
	}
	// Per cell: one cell_start before its first run and one cell_end after its last.
	for _, cell := range []string{"mcp-none-main", "api-none-main"} {
		startAt, endAt, firstRun, lastRun := -1, -1, -1, -1
		for i, ev := range evs {
			if ev["cell"] != cell {
				continue
			}
			switch ev["event"] {
			case "cell_start":
				startAt = i
			case "cell_end":
				endAt = i
			case "run_start":
				if firstRun < 0 {
					firstRun = i
				}
			case "run_end":
				lastRun = i
			}
		}
		if startAt < 0 || startAt > firstRun || endAt < lastRun {
			t.Errorf("cell %s: cell_start %d, first run %d, last run %d, cell_end %d", cell, startAt, firstRun, lastRun, endAt)
		}
	}
	last := eventsOf(evs, "plan_end")[0]
	if p := sub(t, last, "progress"); num(t, p["plan_ended"]) != 8 || num(t, p["plan_total"]) != 8 || num(t, p["percent"]) != 100 || num(t, p["eta_s"]) != 0 {
		t.Fatalf("plan_end progress: %v", p)
	}
	if c := sub(t, last, "counts"); num(t, c["done"]) != 8 {
		t.Fatalf("plan_end counts: %v", c)
	}
	// The first progress event knows a mean and an estimate, plan_start does not (nothing ended yet).
	if p := sub(t, start, "progress"); p["eta_s"] != nil || p["mean_run_s"] != nil {
		t.Fatalf("plan_start progress should have no estimate: %v", p)
	}
	if p := sub(t, eventsOf(evs, "progress")[0], "progress"); num(t, p["plan_ended"]) != 1 || p["eta_s"] == nil || p["eta_basis"] == "" {
		t.Fatalf("first progress: %v", p)
	}
}

// #280 E7: with --cell the cell_start follows plan_start and the cell_end precedes plan_end; the progress still counts the whole plan.
func TestEventsCellStartEndByPosition(t *testing.T) {
	api := newFakeExpAPI()
	r, events, _ := newEventRunner(t, api, func(o *expRunOptions) { o.Cells = []string{"mcp-none-main"} })
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := decodeEvents(t, events)
	names := eventNames(evs)
	want := []string{"plan_start", "cell_start"}
	for i := 0; i < 4; i++ {
		want = append(want, "run_pending", "run_start", "run_end", "progress")
	}
	want = append(want, "cell_end", "plan_end")
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("sequence:\n got %v\nwant %v", names, want)
	}
	end := eventsOf(evs, "cell_end")[0]
	if end["complete"] != true || end["cell"] != "mcp-none-main" {
		t.Fatalf("cell_end: %v", end)
	}
	if c := sub(t, end, "counts"); num(t, c["done"]) != 4 {
		t.Fatalf("cell counts: %v", c)
	}
	if p := sub(t, end, "progress"); num(t, p["cell_ended"]) != 4 || num(t, p["cell_total"]) != 4 || num(t, p["plan_total"]) != 8 || num(t, p["plan_ended"]) != 4 {
		t.Fatalf("cell_end progress: %v", p)
	}
	if num(t, evs[0]["runs"]) != 4 || num(t, evs[0]["cells"]) != 1 {
		t.Fatalf("plan_start: %v", evs[0])
	}
}

// #280 E6: run_end carries status, pass, reason, duration and the tokens of the chat; a run that was not judged has pass null.
func TestRunEndCarriesStatusDurationTokens(t *testing.T) {
	api := newFakeExpAPI()
	api.detailHook = func(id string, det agwclient.ChatDetail) agwclient.ChatDetail {
		det.Chat.Tokens = agwclient.Tokens{Input: 100, Output: 50, CacheRead: 7}
		return det
	}
	r, events, _ := newEventRunner(t, api, nil)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	end := eventsOf(decodeEvents(t, events), "run_end")[0]
	if end["status"] != "done" || end["pass"] != true || end["chat"] != "c1" {
		t.Fatalf("run_end: %v", end)
	}
	if tok := sub(t, end, "tokens"); num(t, tok["input"]) != 100 || num(t, tok["output"]) != 50 || num(t, tok["cache_read"]) != 7 {
		t.Fatalf("tokens: %v", tok)
	}
	if d := num(t, end["duration_s"]); d < 0 {
		t.Fatalf("duration_s %v", d)
	}

	// A run that fails before its chat exists is not judged: pass is null, and the reason is there.
	failing := newFakeExpAPI()
	failing.createErr = errors.New("boom")
	r2, events2, _ := newEventRunner(t, failing, nil)
	if err := r2.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events2.String(), `"pass":null`) {
		t.Fatalf("no pass:null in %s", events2.String())
	}
	end2 := eventsOf(decodeEvents(t, events2), "run_end")[0]
	if end2["status"] != "error" || !strings.Contains(end2["reason"].(string), "chat: boom") {
		t.Fatalf("run_end of the failing run: %v", end2)
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// #280 E5: a failing event writer is reported once and the plan runs on.
func TestEventsWriteErrorReportedOnce(t *testing.T) {
	api := newFakeExpAPI()
	r, _, errw := newEventRunner(t, api, func(o *expRunOptions) { o.Events = failingWriter{} })
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(errw.String(), "events: writing failed"); n != 1 {
		t.Fatalf("reported %d times: %s", n, errw.String())
	}
	if len(api.ends) != 8 {
		t.Fatalf("%d runs ended, want 8", len(api.ends))
	}
}

// #280 E9: a pause file that appears while a run goes on stops the plan before the next run. The run ends regularly.
func TestPauseFileStopsBeforeNextRun(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "pause")
	api := newFakeExpAPI()
	api.detailHook = func(id string, det agwclient.ChatDetail) agwclient.ChatDetail {
		if id == "c1" {
			if err := os.WriteFile(pause, []byte("manual: nightly check\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return det
	}
	r, events, _ := newEventRunner(t, api, func(o *expRunOptions) { o.PauseFile = pause })
	err := r.run(context.Background())
	if !errors.Is(err, errExpPaused) {
		t.Fatalf("run: %v", err)
	}
	if len(api.starts) != 1 || len(api.ends) != 1 {
		t.Fatalf("%d starts, %d ends; the first run must end regularly and no second one start", len(api.starts), len(api.ends))
	}
	if _, statErr := os.Stat(pause); !os.IsNotExist(statErr) {
		t.Fatalf("the pause file was not deleted: %v", statErr)
	}
	evs := decodeEvents(t, events)
	names := eventNames(evs)
	if tail := strings.Join(names[len(names)-2:], " "); tail != "run_pending paused" {
		t.Fatalf("sequence: %v", names)
	}
	if n := len(eventsOf(evs, "run_start")); n != 1 {
		t.Fatalf("%d run_start events, want 1: %v", n, names)
	}
	endAt, pausedAt := -1, -1
	for i, n := range names {
		if n == "run_end" {
			endAt = i
		}
		if n == "paused" {
			pausedAt = i
		}
	}
	if endAt < 0 || pausedAt < endAt {
		t.Fatalf("run_end must come before paused: %v", names)
	}
	paused := evs[pausedAt]
	if paused["reason"] != "manual: nightly check" || sub(t, paused, "progress")["plan_ended"] != float64(1) {
		t.Fatalf("paused: %v", paused)
	}
	var b bytes.Buffer
	if code := (&app{stderr: &b}).exitCode(fmt.Errorf("wrapped: %w", err)); code != 75 {
		t.Fatalf("exit code %d, want 75", code)
	}
}

// #280 E9: --pause-wait delays the look at the pause file; a request that comes during the wait counts.
func TestPauseWaitThenCheck(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "pause")
	time.AfterFunc(50*time.Millisecond, func() { _ = os.WriteFile(pause, []byte("platz"), 0o644) })
	api := newFakeExpAPI()
	r, events, _ := newEventRunner(t, api, func(o *expRunOptions) { o.PauseFile = pause; o.PauseWait = 200 * time.Millisecond })
	began := time.Now()
	err := r.run(context.Background())
	if !errors.Is(err, errExpPaused) {
		t.Fatalf("run: %v", err)
	}
	if took := time.Since(began); took < 190*time.Millisecond {
		t.Fatalf("the runner did not wait: %s", took)
	}
	if len(api.starts) != 0 {
		t.Fatalf("a run started before the pause: %d", len(api.starts))
	}
	if paused := eventsOf(decodeEvents(t, events), "paused"); len(paused) != 1 || paused[0]["reason"] != "platz" {
		t.Fatalf("paused events: %v", paused)
	}
}

// #280 E10: an interrupt aborts the chat of the running run, removes the fixtures and leaves the attempt "started".
func TestInterruptAbortsChatKeepsStarted(t *testing.T) {
	dir := writeRunnerMaterial(t)
	writeRunnerFixtures(t, dir)
	api := newFakeExpAPI()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.pollHook = func(id string, n int, det agwclient.ChatDetail) agwclient.ChatDetail {
		if n == 1 {
			cancel()
		}
		det.Chat.Running = true
		return det
	}
	r, events, _ := newEventRunnerAt(t, api, dir, nil)
	first := r.order[0]
	err := r.run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	if len(api.aborted) != 1 || api.aborted[0] != "c1" {
		t.Fatalf("aborted chats: %v", api.aborted)
	}
	if len(api.ends) != 0 {
		t.Fatalf("the interrupted run was ended: %+v", api.ends)
	}
	if st := api.runs[first.Key].Status; st != "started" {
		t.Fatalf("the attempt is %q, want started", st)
	}
	if api.events[len(api.events)-1] != "unseed" {
		t.Fatalf("the fixtures were not removed after the interrupt: %v", api.events)
	}
	evs := decodeEvents(t, events)
	names := eventNames(evs)
	if names[len(names)-1] != "abort" || len(eventsOf(evs, "plan_end")) != 0 || len(eventsOf(evs, "run_end")) != 0 {
		t.Fatalf("sequence: %v", names)
	}
	abort := evs[len(evs)-1]
	if abort["key"] != first.Key || abort["chat"] != "c1" {
		t.Fatalf("abort: %v", abort)
	}
	var b bytes.Buffer
	if code := (&app{stderr: &b}).exitCode(err); code != 130 {
		t.Fatalf("exit code %d, want 130", code)
	}
}

// startedFirstRun leaves the first run of the plan as an attempt of an earlier call that stayed "started" with a chat.
func startedFirstRun(api *fakeExpAPI, r *expRunner, chat string) expPlannedRun {
	first := r.order[0]
	api.runs[first.Key] = agwclient.ExpRun{ID: first.Key, Key: first.Key, Cell: first.Cell, Task: first.Task, Rep: first.Rep,
		Status: "started", Position: first.Position, ChatID: chat}
	api.chats[chat] = agwclient.ChatDetail{Chat: agwclient.Chat{ID: chat}}
	return first
}

// #280 E11: before an interrupted attempt is superseded its chat is aborted and what it created is removed.
func TestResumeAbortsAndCleansPreviousChat(t *testing.T) {
	api := newFakeExpAPI()
	r, _, _ := newEventRunner(t, api, nil)
	first := startedFirstRun(api, r, "old")
	api.objects["old"] = []agwclient.ExpObject{{Resource: "dataset", ID: "77"}}
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.aborted) == 0 || api.aborted[0] != "old" {
		t.Fatalf("aborted: %v", api.aborted)
	}
	found := false
	for _, p := range api.deletes() {
		found = found || p == "/datasets/77"
	}
	if !found {
		t.Fatalf("the objects of the old chat were not removed: %v", api.deletes())
	}
	if len(api.supersedes) != 1 || api.supersedes[0] != [2]string{first.Key, first.Key + "#2"} {
		t.Fatalf("supersede: %v", api.supersedes)
	}
}

// #280 E7: resumed comes after plan_start when runs ended before or an attempt is replaced, and names both.
func TestResumeEmitsResumed(t *testing.T) {
	api := newFakeExpAPI()
	r, events, _ := newEventRunner(t, api, nil)
	done, started := r.order[0], r.order[1]
	api.runs[done.Key] = agwclient.ExpRun{ID: done.Key, Key: done.Key, Cell: done.Cell, Task: done.Task, Rep: done.Rep, Status: "done",
		StartedAt: "2026-11-28T00:00:00Z", EndedAt: "2026-11-28T00:10:00Z"}
	api.runs[started.Key] = agwclient.ExpRun{ID: started.Key, Key: started.Key, Cell: started.Cell, Task: started.Task, Rep: started.Rep,
		Status: "started", ChatID: "old"}
	api.chats["old"] = agwclient.ChatDetail{Chat: agwclient.Chat{ID: "old"}}
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := decodeEvents(t, events)
	if names := eventNames(evs); names[0] != "plan_start" || names[1] != "resumed" {
		t.Fatalf("sequence: %v", names[:3])
	}
	if num(t, evs[0]["ended_before"]) != 1 {
		t.Fatalf("plan_start: %v", evs[0])
	}
	res := evs[1]
	if num(t, res["ended_before"]) != 1 || fmt.Sprint(res["superseded"]) != "["+started.Key+"]" {
		t.Fatalf("resumed: %v", res)
	}
	// Seven runs are open, each estimated with the 600 s of the only ended run; the other task has no mean of its own.
	if p := sub(t, res, "progress"); num(t, p["plan_ended"]) != 1 || num(t, p["mean_run_s"]) != 600 || num(t, p["eta_s"]) != 4200 || p["eta_basis"] != "plan" {
		t.Fatalf("resumed progress: %v", p)
	}
	// A call that starts from nothing emits no resumed.
	fresh := newFakeExpAPI()
	r2, events2, _ := newEventRunner(t, fresh, nil)
	if err := r2.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(eventsOf(decodeEvents(t, events2), "resumed")); n != 0 {
		t.Fatalf("%d resumed events for a fresh plan", n)
	}
}

// #280 E6: the plan limit stops the plan with an error event of kind plan_limit.
func TestPlanLimitEmitsError(t *testing.T) {
	api := newFakeExpAPI()
	api.usage = agwclient.ExpUsage{TokensTotal: 20000, DurationTotal: "1m", Runs: 1}
	r, events, _ := newEventRunner(t, api, nil)
	if err := r.run(context.Background()); err == nil {
		t.Fatal("the plan should stop at its limit")
	}
	evs := decodeEvents(t, events)
	errEv := evs[len(evs)-1]
	if errEv["event"] != "error" || errEv["kind"] != "plan_limit" || !strings.Contains(errEv["message"].(string), "plan limit reached") {
		t.Fatalf("last event: %v", errEv)
	}
	if len(eventsOf(evs, "plan_end")) != 0 {
		t.Fatal("plan_end after an error")
	}
}

// #280 E6: a model the catalogue does not know ends the call before any chat, as an error of kind environment.
func TestUnknownModelEmitsEnvironmentError(t *testing.T) {
	api := newFakeExpAPI()
	api.models = []agwclient.Model{{ID: "other/model", Pricing: &agwclient.Pricing{Input: 1, Output: 1}}}
	r, events, _ := newEventRunner(t, api, nil)
	if err := r.run(context.Background()); err == nil {
		t.Fatal("an unknown model must stop the run")
	}
	evs := decodeEvents(t, events)
	if len(evs) != 1 || evs[0]["event"] != "error" || evs[0]["kind"] != "environment" {
		t.Fatalf("events: %v", evs)
	}
	if len(api.starts) != 0 || len(api.creates) != 0 {
		t.Fatalf("a run started: %d starts, %d chats", len(api.starts), len(api.creates))
	}
}

// #280 E12: every line of a stream gets the UTC time; a line that arrives in pieces gets it once.
func TestTimestampsPrefixLines(t *testing.T) {
	var b bytes.Buffer
	now := time.Date(2026, 11, 28, 7, 12, 3, 999, time.UTC)
	w := &timestampWriter{w: &b, now: func() time.Time { return now }}
	fmt.Fprint(w, "first\nsecond ")
	now = now.Add(2 * time.Second)
	fmt.Fprint(w, "half\nthird\n")
	want := "2026-11-28T07:12:03Z first\n2026-11-28T07:12:03Z second half\n2026-11-28T07:12:05Z third\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

// #280 E3: AGW_EXP_EVENTS is the default of --events and the switch wins; "-" moves the text lines to stderr.
func TestEnvVarsAndFlagPrecedence(t *testing.T) {
	dir := writeRunnerMaterial(t)
	tmp := t.TempDir()
	fromEnv, fromFlag := filepath.Join(tmp, "env.jsonl"), filepath.Join(tmp, "flag.jsonl")
	getenv := func(k string) string {
		if k == "AGW_EXP_EVENTS" {
			return fromEnv
		}
		return ""
	}
	run := func(args ...string) (int, string, string) {
		var out, errw strings.Builder
		code := realMain(context.Background(), append([]string{"exp", "run", "--plan", dir, "--dry-run"}, args...), strings.NewReader(""), &out, &errw, getenv)
		return code, out.String(), errw.String()
	}
	if code, _, errw := run(); code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	if _, err := os.Stat(fromEnv); err != nil {
		t.Fatalf("AGW_EXP_EVENTS was not used: %v", err)
	}
	_ = os.Remove(fromEnv)
	if code, _, errw := run("--events", fromFlag); code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	if _, err := os.Stat(fromFlag); err != nil {
		t.Fatalf("--events was not used: %v", err)
	}
	if _, err := os.Stat(fromEnv); err == nil {
		t.Fatal("the environment won over the switch")
	}
	code, stdout, stderr := run("--events", "-")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "runs (k=2)") {
		t.Fatalf("--events -: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

// The runner's side of the events, the pause request and the clean-up of an interrupted attempt (issue #280).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"agw/internal/agwclient"
)

// errExpPaused ends the runner after a pause request (exit code 75). The attempt in progress was never touched.
var errExpPaused = errors.New("paused on request")

// exitPaused is the exit code of a runner that stopped on a pause request (EX_TEMPFAIL).
const exitPaused = 75

// expCurrent is the attempt the runner is working on, for the abort event.
type expCurrent struct {
	pr   expPlannedRun
	id   string
	chat string
}

// expKindErr tags an error with the kind its error event carries. The message stays the inner one.
type expKindErr struct {
	kind string
	err  error
}

func (e *expKindErr) Error() string { return e.err.Error() }
func (e *expKindErr) Unwrap() error { return e.err }

// kindErr tags err with kind; nil stays nil, and a tag already there wins.
func kindErr(kind string, err error) error {
	if err == nil {
		return nil
	}
	var ke *expKindErr
	if errors.As(err, &ke) {
		return err
	}
	return &expKindErr{kind: kind, err: err}
}

func kindOf(err error) string {
	var ke *expKindErr
	if errors.As(err, &ke) {
		return ke.kind
	}
	return kindOther
}

func (r *expRunner) currentChat() string {
	if r.current == nil {
		return ""
	}
	return r.current.chat
}

// cellBounds gives the index of the first and the last run of every cell in the order.
func cellBounds(order []expPlannedRun) (first, last map[string]int) {
	first, last = map[string]int{}, map[string]int{}
	for i, pr := range order {
		if _, ok := first[pr.Cell]; !ok {
			first[pr.Cell] = i
		}
		last[pr.Cell] = i
	}
	return first, last
}

func (r *expRunner) runBase(name string, pr expPlannedRun, attempt, chat string) expEvent {
	return expEvent{Event: name, Cell: pr.Cell, Task: pr.Task, Rep: pr.Rep, Key: pr.Key, Attempt: attempt, Chat: chat}
}

func (r *expRunner) progressView(cell string) *expProgressView {
	if r.latest == nil {
		return nil
	}
	v := expProgress(r.fullOrder, r.latest, cell)
	return &v
}

func (r *expRunner) emitPlanStart(existing map[string]agwclient.ExpRun, endedBefore int) {
	if r.emit == nil {
		return
	}
	cells := map[string]bool{}
	var superseded []string
	for _, pr := range r.order {
		cells[pr.Cell] = true
		if prev, ok := existing[pr.Key]; ok && prev.Status == expStarted {
			superseded = append(superseded, prev.ID)
		}
	}
	runs, lock := len(r.order), r.lock != nil
	seed := r.seed
	r.emit.Emit(expEvent{Event: evPlanStart, Cells: len(cells), K: r.k, Seed: &seed, Runs: &runs, Lock: &lock,
		EndedBefore: &endedBefore, Progress: r.progressView("")})
	if endedBefore > 0 || len(superseded) > 0 {
		r.emit.Emit(expEvent{Event: evResumed, EndedBefore: &endedBefore, Superseded: superseded, Progress: r.progressView("")})
	}
}

func (r *expRunner) emitPlanEnd(executed, skipped int, took time.Duration) {
	if r.emit == nil {
		return
	}
	counts := expPlanCounts(r.fullOrder, r.latest)
	secs := roundTenth(took.Seconds())
	r.emit.Emit(expEvent{Event: evPlanEnd, Runs: &executed, Skipped: &skipped, DurationS: &secs, Progress: r.progressView(""), Counts: &counts})
}

// emitCell reports the first (cell_start) or last (cell_end) run of a cell in this call's order. cell_end carries the
// counts of the cell and complete, true when every planned run of the cell has ended.
func (r *expRunner) emitCell(name, cell string, end bool) {
	if r.emit == nil {
		return
	}
	ev := expEvent{Event: name, Cell: cell, Progress: r.progressView(cell)}
	if end {
		counts := expCellCounts(r.fullOrder, r.latest, cell)
		complete := ev.Progress.CellEnded == ev.Progress.CellTotal
		ev.Counts, ev.Complete = &counts, &complete
	}
	r.emit.Emit(ev)
}

func (r *expRunner) emitRunPending(pr expPlannedRun, prev agwclient.ExpRun, hasPrev bool) {
	if r.emit == nil {
		return
	}
	id := pr.Key
	if hasPrev {
		id = fmt.Sprintf("%s#%d", pr.Key, expAttempt(prev.ID, pr.Key)+1)
	}
	r.emit.Emit(r.runBase(evRunPending, pr, id, ""))
}

func (r *expRunner) emitRun(name string, pr expPlannedRun, id, chat string) {
	if r.emit == nil {
		return
	}
	r.emit.Emit(r.runBase(name, pr, id, chat))
}

func (r *expRunner) emitRunEnd(pr expPlannedRun, id, chat, status, reason string, pass *bool, took time.Duration, tokens *expEventTokens) {
	if r.emit == nil {
		return
	}
	ev := r.runBase(evRunEnd, pr, id, chat)
	secs := roundTenth(took.Seconds())
	ev.Status, ev.Pass, ev.Reason, ev.DurationS, ev.Tokens = status, passJSON(pass), truncate(reason, 300), &secs, tokens
	r.emit.Emit(ev)
}

func (r *expRunner) emitProgress(cell string) {
	if r.emit == nil {
		return
	}
	counts := expPlanCounts(r.fullOrder, r.latest)
	r.emit.Emit(expEvent{Event: evProgress, Cell: cell, Progress: r.progressView(cell), Counts: &counts})
}

// emitStop reports the event that ends the call without finishing the plan: error (with kind and message) or abort
// (with the attempt that was interrupted).
func (r *expRunner) emitStop(name string, err error) {
	if r.emit == nil {
		return
	}
	ev := expEvent{Event: name, Progress: r.progressView("")}
	if r.current != nil {
		ev = r.runBase(name, r.current.pr, r.current.id, r.current.chat)
		ev.Progress = r.progressView("")
	}
	if name == evError {
		ev.Kind, ev.Message = kindOf(err), truncate(err.Error(), 500)
	}
	r.emit.Emit(ev)
}

func roundTenth(v float64) float64 { return math.Round(v*10) / 10 }

// noteEnded records an attempt that ended in this call, so that the progress counts it and its duration without a
// second read of the protocol.
func (r *expRunner) noteEnded(pr expPlannedRun, out expOutcome) {
	r.latest[pr.Key] = agwclient.ExpRun{
		ID: out.id, Key: pr.Key, Plan: pr.Plan, Cell: pr.Cell, Task: pr.Task, Rep: pr.Rep, Status: out.status, Pass: out.pass,
		StartedAt: out.started.UTC().Format(time.RFC3339Nano), EndedAt: out.ended.UTC().Format(time.RFC3339Nano),
	}
}

// pauseCheck is the pause point before a run (E9): wait, then look for the pause file. The run in progress is never
// interrupted; a request that arrives while a run goes on is seen before the next one.
func (r *expRunner) pauseCheck(ctx context.Context, pr expPlannedRun) error {
	if r.pauseWait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.pauseWait):
		}
	}
	if r.pauseFile == "" {
		return nil
	}
	if _, err := os.Stat(r.pauseFile); err != nil {
		return nil
	}
	data, _ := os.ReadFile(r.pauseFile)
	reason := truncate(strings.Join(strings.Fields(string(data)), " "), 200)
	if err := os.Remove(r.pauseFile); err != nil {
		fmt.Fprintf(r.errw, "the pause file could not be removed: %v\n", err)
	}
	fmt.Fprintf(r.out, "paused before run %s on request: %s\n", pr.Key, orDefault(reason, "no reason given"))
	r.emit.Emit(expEvent{Event: evPaused, Reason: reason, Progress: r.progressView("")})
	return errExpPaused
}

// abortInterrupted stops the chat of an attempt the runner gives up on (E10). It runs after the interrupt cancelled the
// context, so it has a short time of its own.
func (r *expRunner) abortInterrupted(ctx context.Context, chatID string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	r.abort(actx, chatID)
}

// settleInterrupted deals with an attempt that an earlier call left "started" (E11): abort its chat, wait until the chat
// is quiet and remove what it created, so that it neither holds the slot of the new attempt nor leaves objects that
// would change the next count. Failures are only reported; the new attempt starts anyway.
func (r *expRunner) settleInterrupted(ctx context.Context, pr expPlannedRun, prev agwclient.ExpRun) {
	if prev.ChatID == "" {
		return
	}
	fmt.Fprintf(r.out, "run %s: the chat %s of the interrupted attempt is aborted and cleaned up\n", pr.Key, shortID(prev.ChatID))
	r.abort(ctx, prev.ChatID)
	var det agwclient.ChatDetail
	deadline := r.now().Add(r.supersedeWait)
	for {
		d, err := r.api.Chat(ctx, prev.ChatID)
		if err != nil {
			fmt.Fprintf(r.errw, "run %s: chat %s not readable: %v\n", pr.Key, shortID(prev.ChatID), err)
			break
		}
		det = d
		if c := d.Chat; !c.Running && !c.Resuming && c.Queued == 0 {
			break
		}
		if !r.now().Before(deadline) {
			fmt.Fprintf(r.errw, "run %s: chat %s is still running %s after the abort\n", pr.Key, shortID(prev.ChatID), roundDuration(r.supersedeWait))
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.poll):
		}
	}
	if err := r.cleanup(ctx, prev.ChatID, det); err != nil {
		fmt.Fprintf(r.errw, "run %s: cleanup of the interrupted attempt: %v\n", pr.Key, err)
	}
}

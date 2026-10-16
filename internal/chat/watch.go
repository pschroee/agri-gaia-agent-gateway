// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

// Observed platform jobs (autonomy A-E2, D2.1). A platform call that starts a long job (a training, an
// image build, a task) does not end its plan step: the gateway records the job in watch_jobs, the step
// goes to "waiting", and the loop in this file polls the platform with the user's token until the job
// ends (D2.3). Then the step is done or failed, the plan continues or fails, and the agent learns it
// through a note (D2.5) — also out of a resting chat, which this chat's wake-up resumes (D2.6).
//
// The agent never reports progress: it only starts the call, and everything the loop writes down comes
// from the platform (D0.3). Outside a plan a job is only observed for the cards (no note, no wake-up):
// the agent follows such a job itself, like today (E2.2).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"agw/internal/artifacts"
	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// Watching of jobs: how often the loop looks (tick), when a job is due again by its age (D2.3) and how
// many reading calls of the watcher may be on their way at the same time across all chats.
const (
	watchTick       = 5 * time.Second
	watchIntervalA  = 10 * time.Second // the first 2 minutes
	watchIntervalB  = 30 * time.Second // up to 30 minutes
	watchIntervalC  = 60 * time.Second // from 30 minutes on
	watchYoungUntil = 2 * time.Minute
	watchOldFrom    = 30 * time.Minute
	watchParallel   = 4
	// watchFailReads: so many reading calls in a row may fail before the job says so (D2.3).
	watchFailReads = 5
)

// watchInterval is the distance between two polls of a job of this age.
func watchInterval(age time.Duration) time.Duration {
	switch {
	case age < watchYoungUntil:
		return watchIntervalA
	case age < watchOldFrom:
		return watchIntervalB
	default:
		return watchIntervalC
	}
}

// watchNow is the clock of the watcher; tests move it forward so that a job that was just polled is due
// again (the loop itself polls a young job only every watchIntervalA).
var watchNow = time.Now

// watchDue reports whether a job is due again at now: first poll at once, then by its age.
func watchDue(j store.WatchJob, now time.Time) bool {
	if j.CheckedAt == nil || j.CheckedAt.IsZero() {
		return true
	}
	return !now.Before(j.CheckedAt.Add(watchInterval(now.Sub(j.StartedAt))))
}

// watchKind returns the kind registered under the name of the job; a kind that this build does not know
// (a job of a feature that is off, or of another plan) is not polled.
func watchKind(name string) (watch.Kind, bool) {
	return watch.Lookup(name)
}

// RunWatchers is the loop of the gateway: it observes every open job of every chat until it ends. It
// runs until ctx is done (the orchestrator's lifetime).
func (m *Manager) RunWatchers(ctx context.Context) {
	if m.opt.Platform == nil {
		slog.Info("watchers off (no platform binding)")
		return
	}
	t := time.NewTicker(watchTick)
	defer t.Stop()
	sem := make(chan struct{}, watchParallel)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.watchOnce(ctx, sem)
		}
	}
}

// watchOnce polls every job that is due. At most watchParallel reads run at the same time.
func (m *Manager) watchOnce(ctx context.Context, sem chan struct{}) {
	jobs, err := m.st.OpenWatchJobs(ctx)
	if err != nil {
		slog.Warn("open jobs not readable", "error", err)
		return
	}
	now := watchNow()
	var wg sync.WaitGroup
	for _, j := range jobs {
		if !watchDue(j, now) {
			continue
		}
		kind, ok := watchKind(j.Kind)
		if !ok || kind.Poll == nil {
			slog.Warn("unknown kind of an observed job", "job", j.ID, "kind", j.Kind)
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(j store.WatchJob, kind watch.Kind) {
			defer wg.Done()
			defer func() { <-sem }()
			m.pollJob(ctx, j, kind)
		}(j, kind)
	}
	wg.Wait()
}

// pollJob makes one reading call for a job and writes down what it found. A reading that fails does not
// lose the job (D2.3): the row counts the failure, and from watchFailReads in a row on it says so and
// asks the user for attention.
func (m *Manager) pollJob(ctx context.Context, j store.WatchJob, kind watch.Kind) {
	read := m.summaryReader(j.ChatID)
	if read == nil {
		return
	}
	// The watcher reads, it never writes (issue #99, security): only GETs on the paths of the kind go
	// out, and anything else is refused here whatever the kind asks for.
	reader := watch.Reader(func(ctx context.Context, r platform.Request) (platform.Result, error) {
		if r.Method != http.MethodGet || r.Writes() {
			return platform.Result{Status: "error", Message: "the watcher only reads"}, nil
		}
		return read(ctx, r)
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p, err := kind.Poll(ctx, watch.Job{ID: j.ID, ChatID: j.ChatID, Kind: j.Kind, Ref: j.Ref}, reader)
	if err != nil {
		m.jobReadFailed(ctx, j, err)
		return
	}
	if p.Next != nil {
		// The task only started another job: the same row is observed on as that job (the chain of a
		// started training, see internal/watch/training.go).
		if ok, rerr := m.st.ReArmWatchJob(ctx, j.ID, p.Next.Kind, p.Next.Ref); rerr != nil {
			slog.Error("job not handed over to its chained kind", "job", j.ID, "kind", p.Next.Kind, "error", rerr)
		} else if ok {
			slog.Info("job observed on as another kind", "job", j.ID, "chat", j.ChatID, "kind", p.Next.Kind, "ref", p.Next.Ref)
			j.Kind, j.Ref = p.Next.Kind, p.Next.Ref
		}
		m.publishJob(ctx, j, p)
		return
	}
	prog := store.WatchProgress{
		State:   p.State,
		Percent: p.Percent,
		Message: watch.ClipRunes(p.Message, watch.MaxMessage),
		LogTail: watch.Tail(p.LogTail),
		Outputs: storeOutputs(p.Outputs),
		Link:    p.Link,
		ETA:     m.jobETA(j, p, time.Now()),
	}
	if err := m.st.SetJobProgress(ctx, j.ID, prog); err != nil {
		slog.Error("job progress not stored", "job", j.ID, "error", err)
		return
	}
	j.Progress, j.Message, j.LogTail, j.ETA = prog.Percent, prog.Message, prog.LogTail, prog.ETA
	j.Outputs = prog.Outputs
	j.Link = prog.Link
	j.CheckedAt = timePtr(time.Now())
	j.State = prog.State
	m.publishJob(ctx, j, p)
	if prog.State != watch.StateRunning {
		m.finishWatched(ctx, j, p)
	}
}

// jobReadFailed counts a reading call that failed and, from watchFailReads in a row on, writes the
// reason onto the job without ending it (the platform may come back, D2.3).
func (m *Manager) jobReadFailed(ctx context.Context, j store.WatchJob, err error) {
	if uerr := m.st.SetJobProgress(ctx, j.ID, store.WatchProgress{ReadFail: true}); uerr != nil {
		slog.Error("failed read not counted", "job", j.ID, "error", uerr)
		return
	}
	fails := j.FailReads + 1
	if fails < watchFailReads {
		slog.Warn("job not readable", "job", j.ID, "kind", j.Kind, "fails", fails, "error", err)
		return
	}
	msg := "platform not readable: " + oneLine(err.Error())
	if uerr := m.st.SetJobMessage(ctx, j.ID, watch.ClipRunes(msg, watch.MaxMessage)); uerr != nil {
		slog.Error("job message not stored", "job", j.ID, "error", uerr)
	}
	slog.Warn("job still not readable", "job", j.ID, "kind", j.Kind, "fails", fails, "error", err)
	if j.PlanID != "" {
		// The plan step keeps waiting; the user should see that the gateway cannot look right now.
		m.publish(j.ChatID, Event{Kind: "attention", Data: map[string]string{"chat_id": j.ChatID, "job_id": j.ID, "message": msg}})
		if fails == watchFailReads {
			// Once per run of failures (A-E4, D4.3): the read loop calls this on every further attempt,
			// and a notification for each one would be noise. The counter starts over after a readable
			// answer, so a second outage reports again.
			m.publishPlanAttention(j.ChatID, j.PlanID, msg)
		}
	}
}

// jobETA estimates the end of a job from the measurements the loop has: its start, what was recorded
// when it was checked last and what the platform says now (D2.4).
func (m *Manager) jobETA(j store.WatchJob, p watch.Progress, now time.Time) *time.Time {
	if p.Percent == nil {
		return nil
	}
	samples := []watch.Sample{{At: now, Percent: *p.Percent}}
	if j.CheckedAt != nil && j.Progress != nil {
		samples = append([]watch.Sample{{At: *j.CheckedAt, Percent: *j.Progress}}, samples...)
	}
	if len(samples) < 2 {
		return nil
	}
	return watch.ETA(j.StartedAt, samples)
}

// storeOutputs turns the outputs of a poll into the stored form.
func storeOutputs(out []watch.Output) []store.WatchOutput {
	if len(out) == 0 {
		return nil
	}
	res := make([]store.WatchOutput, 0, len(out))
	for _, o := range out {
		res = append(res, store.WatchOutput{Resource: o.Resource, ID: o.ID, Label: o.Label})
	}
	return res
}

// publishJob distributes a job's new state to the subscribers of its chat.
func (m *Manager) publishJob(ctx context.Context, j store.WatchJob, p watch.Progress) {
	d := JobEvent{Change: "progress", Job: j}
	if p.Next != nil {
		d.Change = "next"
	}
	if j.State != watch.StateRunning {
		d.Change = "ended"
	}
	m.publish(j.ChatID, Event{Kind: "job", Data: d})
	if j.PlanID != "" {
		m.publishPlan(ctx, j.PlanID)
	}
}

// JobEvent is the SSE event "job": the state of one observed platform job of the chat.
type JobEvent struct {
	// Change: progress (the job runs on), next (the job is observed on as another kind), ended.
	Change string         `json:"change"`
	Job    store.WatchJob `json:"job"`
}

// startWatching records the job a plan step started and moves the step to "waiting". It returns the id
// of the job the step now waits for; "" when the job could not be stored — then the caller finishes the
// step as done, so that a plan never hangs on a row that does not exist.
func (m *Manager) startWatching(ctx context.Context, chatID string, st *store.PlanStep, from, kind, ref, result, resource, id string) string {
	jobID, err := m.st.AddWatchJob(ctx, store.WatchJob{ChatID: chatID, PlanID: st.PlanID, StepIdx: st.Idx,
		ToolCallID: store.ToolCallFrom(ctx), Kind: kind, Ref: ref})
	if err != nil {
		slog.Error("the job a plan step started is not observed", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "kind", kind, "error", err)
		return ""
	}
	ok, err := m.st.WaitPlanStep(ctx, st.PlanID, st.Idx, jobID, result, resource, id)
	if err != nil || !ok {
		slog.Error("plan step does not wait for its job", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "job", jobID, "error", err)
		return ""
	}
	slog.Info("plan step waits for a platform job", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "job", jobID, "kind", kind, "ref", ref)
	m.publishPlan(ctx, st.PlanID)
	return jobID
}

// watchNote is the sentence the answer of a call that started an observed job adds (A-E2, D2.1): the
// gateway watches the job with the user's token, so the agent does not have to hold its turn open for it
// and poll the platform itself — the live run of 16.10.2026 showed that it does exactly that without a
// word, which keeps the sandbox alive and the chat active for the whole job.
//
// The promise "you will be woken" is only made when it is kept (D2.6): an autonomous plan that is
// approved wakes the agent when its step ends, any other plan does not, and then the agent keeps
// following the job itself like before A-E2.
func (m *Manager) watchNote(ctx context.Context, st *store.PlanStep, jobID string) string {
	woken := false
	if p, err := m.st.GetPlan(ctx, st.PlanID); err == nil {
		woken = p.Mode == plan.ModeAutonomous && p.State == plan.StateApproved
	}
	if woken {
		return fmt.Sprintf("The gateway observes this platform job (job %s) for step %d and wakes you when it ends; "+
			"do not poll the job yourself — end your turn.", jobID, st.Idx)
	}
	return fmt.Sprintf("The gateway observes this platform job (job %s) for step %d; its state and the step's are in "+
		"GET /api/chats/<chat>/jobs. This plan does not wake you, so keep an eye on it yourself.", jobID, st.Idx)
}

// recordJobOutsidePlan observes a call that started a job without a plan (A-E2.2): the row is there for
// the cards and the activity, the agent gets no note and no wake-up.
func (m *Manager) recordJobOutsidePlan(ctx context.Context, chatID string, access delegation.Access, req platform.Request, res platform.Result) {
	kind, ref, ok := watch.For(access, req, res)
	if !ok {
		return
	}
	id, err := m.st.AddWatchJob(ctx, store.WatchJob{ChatID: chatID, ToolCallID: store.ToolCallFrom(ctx), Kind: kind, Ref: ref})
	if err != nil {
		slog.Error("started job not observed", "chat", chatID, "kind", kind, "error", err)
		return
	}
	slog.Info("job observed without a plan", "chat", chatID, "job", id, "kind", kind, "ref", ref)
}

// finishWatched ends a plan step whose job ended: the step is done or failed (the retry of A-E1 applies
// for a covered step), the plan completes or fails, and the agent is told. A job outside a plan is only
// closed in its row.
func (m *Manager) finishWatched(ctx context.Context, j store.WatchJob, p watch.Progress) {
	if j.PlanID == "" {
		return
	}
	dur := time.Since(j.StartedAt)
	pl, err := m.st.GetPlan(ctx, j.PlanID)
	if err != nil {
		slog.Error("plan of a watched step not readable", "plan", j.PlanID, "error", err)
		return
	}
	st := stepOf(pl, j.StepIdx)
	if st == nil || st.State != plan.StepWaiting {
		// The step does not wait any more (the plan ended in between, and its steps were skipped): the
		// job's row is already closed by the poll that got here, and no step and nobody else is touched.
		slog.Info("job of a plan that does not wait for it ended", "job", j.ID, "plan", j.PlanID, "step", j.StepIdx)
		return
	}
	to, result := plan.StepDone, fmt.Sprintf("job %s done", j.ID)
	if p.State == watch.StateFailed {
		to, result = plan.StepFailed, fmt.Sprintf("job %s failed", j.ID)
	}
	ok, err := m.st.FinishPlanStep(ctx, j.PlanID, j.StepIdx, plan.StepWaiting, to, result, st.CreatedResource, st.CreatedID)
	if err != nil || !ok {
		slog.Error("watched step not finished", "plan", j.PlanID, "step", j.StepIdx, "error", err)
		return
	}
	if to == plan.StepFailed {
		retried := false
		if st.Covered {
			var rerr error
			retried, rerr = m.st.RetryPlanStep(ctx, j.PlanID, j.StepIdx)
			if rerr != nil {
				slog.Error("watched step not retried", "plan", j.PlanID, "step", j.StepIdx, "error", rerr)
			}
		}
		if !retried {
			if _, err := m.st.FinishPlan(ctx, j.PlanID, plan.StateFailed); err != nil {
				slog.Error("plan not failed", "plan", j.PlanID, "error", err)
			}
		}
	} else if fresh, err := m.st.GetPlan(ctx, j.PlanID); err == nil && allDone(fresh) {
		if _, err := m.st.FinishPlan(ctx, j.PlanID, plan.StateCompleted); err != nil {
			slog.Error("plan not completed", "plan", j.PlanID, "error", err)
		}
	}
	m.publishPlan(ctx, j.PlanID)
	// Whether this note may wake the chat is decided by the plan as it was before this step ended: a step
	// that finishes the plan completes it right above, and that note is exactly the one the user waits for.
	m.noteJobEnd(ctx, j, st, p, dur, pl.Mode == plan.ModeAutonomous && pl.State == plan.StateApproved)
}

// stepOf finds a step of a plan by its index.
func stepOf(p store.Plan, idx int) *store.PlanStep {
	for i := range p.Steps {
		if p.Steps[i].Idx == idx {
			return &p.Steps[i]
		}
	}
	return nil
}

// JobNote is the note to the agent at the end of an observed plan step (D2.5). The head is built by the
// gateway from its own data, the platform's message and the log tail go below it into the fence (they
// are foreign content, see origin.go).
func JobNote(j store.WatchJob, st *store.PlanStep, state string, dur time.Duration) systemNote {
	var h strings.Builder
	if st != nil {
		fmt.Fprintf(&h, "Plan step %d %q ", st.Idx, watch.ClipRunes(oneLine(st.Description), 60))
	} else {
		fmt.Fprintf(&h, "Platform job %s ", j.ID)
	}
	phrase := "finished"
	if state == watch.StateFailed {
		phrase = "failed"
	}
	h.WriteString(phrase + " (" + j.Kind + ") after " + FormatRuntime(dur) + ".")
	var b strings.Builder
	if j.Message != "" {
		b.WriteString("Platform message: " + watch.ClipRunes(oneLine(j.Message), watch.MaxMessage))
	}
	tail := watch.Tail(j.LogTail)
	if tail == "" {
		if b.Len() == 0 {
			b.WriteString("The platform reported no message and no log.")
		}
	} else {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Log (last " + fmt.Sprint(countLines(tail)) + " lines):\n" + tail)
	}
	refs := []string{j.ID}
	if st != nil {
		refs = append(refs, fmt.Sprintf("%d", st.Idx))
	}
	return systemNote{Type: store.NotePlanStep, Refs: refs, Summary: h.String(), Body: b.String()}
}

// countLines counts the lines of a text (for the note's head).
func countLines(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// noteJobEnd tells the agent that a watched step ended (D2.5) and wakes a resting chat (D2.6). wake is
// false for a plan that was not approved while the step ran: its note stays in the queue and goes with
// the next message.
func (m *Manager) noteJobEnd(ctx context.Context, j store.WatchJob, st *store.PlanStep, p watch.Progress, dur time.Duration, wake bool) {
	if j.PlanID == "" {
		return
	}
	note := JobNote(j, st, p.State, dur)
	if _, err := m.st.EnqueueSystem(ctx, j.ChatID, note.Type, note.Refs, note.Text()); err != nil {
		slog.Error("note about the end of a job not queued", "chat", j.ChatID, "job", j.ID, "error", err)
		return
	}
	m.publishQueue(ctx, j.ChatID, "queued", nil, "")
	if !wake {
		m.wakeIfLive(j.ChatID)
		return
	}
	m.wakeForNote(j.ChatID, j.PlanID)
}

// wakeForNote hands a note of a plan to the agent: a live chat that is not working gets it at once, a
// resting one is resumed for it (D2.6). Only ever called for a note that earns a turn — the end of a
// plan step or the user's decision on a parked step.
func (m *Manager) wakeForNote(chatID, planID string) {
	m.mu.Lock()
	l := m.live[chatID]
	busy := m.sending[chatID] || (l != nil && (l.running || l.compacting))
	m.mu.Unlock()
	if l != nil {
		if !busy {
			m.deliverQueue(chatID, l)
		}
		return
	}
	m.wakeDormant(chatID, planID)
}

// wakeIfLive hands notes to a live chat that is not working; a resting chat stays resting (only an
// autonomous plan wakes one, D2.6).
func (m *Manager) wakeIfLive(chatID string) {
	m.mu.Lock()
	l := m.live[chatID]
	busy := m.sending[chatID] || (l != nil && (l.running || l.compacting))
	m.mu.Unlock()
	if l != nil && !busy {
		m.deliverQueue(chatID, l)
	}
}

// wakeDormant resumes a resting chat because a plan step ended (D2.6) and delivers the note afterwards.
// It goes through ensureLive, the same path a resume on opening the chat takes (the SSE event "resume"
// reports its steps to the UI); the note lies in the queue and deliverQueue hands it to pi.
func (m *Manager) wakeDormant(chatID, planID string) {
	go func() {
		ctx := context.Background()
		l, _, err := m.ensureLive(ctx, chatID)
		if err != nil {
			slog.Warn("chat of a finished plan step not woken", "chat", chatID, "plan", planID, "error", err)
			return
		}
		slog.Info("wake-up by a finished plan step", "chat", chatID, "plan", planID)
		m.deliverQueue(chatID, l)
	}()
}

// --- Parked approvals (D2.8) ---
//
// A step of an autonomous plan that the plan does not cover (irreversible, confirm each) is not waited
// for in the running turn: the approval stays open until the plan expires, the call answers at once and
// the agent ends its turn. A goroutine of the gateway holds the wait; on approval it repeats exactly
// the stored call through PlatformCall, so that delegation, field rules, the compute limit and the plan
// checks apply again, and wakes the agent with the result.
//
// The marker below is set by that goroutine alone; no request, tool or header can set it (a test holds
// that).

type parkedApprovalKey struct{}

// WithParkedApproval marks a context as the gateway's own repetition of a parked step.
func WithParkedApproval(ctx context.Context, approvalID string) context.Context {
	return context.WithValue(ctx, parkedApprovalKey{}, approvalID)
}

// parkedApprovalFrom returns the parked approval of the context; "" when the call is not a repetition.
func parkedApprovalFrom(ctx context.Context) string {
	s, _ := ctx.Value(parkedApprovalKey{}).(string)
	return s
}

// parkedHere reports whether this call is the gateway repeating a parked step (D2.8): the marker names
// the approval, a step of this chat carries exactly that approval, and the call is exactly that step.
// The marker alone never opens a call — the step and the call have to match as well, so a marker that
// leaks into another call asks the user again.
func (m *Manager) parkedHere(ctx context.Context, chatID string, req platform.Request) bool {
	marker := parkedApprovalFrom(ctx)
	if marker == "" {
		return false
	}
	st, err := m.st.PlanStepByParkedApproval(ctx, chatID, marker)
	if err != nil {
		return false
	}
	p, err := m.st.GetPlan(ctx, st.PlanID)
	if err != nil {
		return false
	}
	resolve := func(n int) (string, bool) {
		ref := stepOf(p, n)
		if ref == nil || ref.State != plan.StepDone || ref.CreatedID == "" {
			return "", false
		}
		ok, err := m.st.IsDelegationObject(ctx, p.ChatID, ref.CreatedResource, ref.CreatedID)
		return ref.CreatedID, err == nil && ok
	}
	if plan.IsToolStep(st) {
		inv, has := platform.InvocationFrom(ctx)
		if !has {
			return false
		}
		ok, _ := plan.MatchesTool(st, inv.Tool, inv.Args, req, resolve)
		return ok
	}
	ok, _ := plan.Matches(st, req, resolve)
	return ok
}

// stepIsParked reports whether a step of an autonomous plan is parked instead of waited for (D2.8):
// the plan must be autonomous and still running, and the step still open.
func (m *Manager) stepIsParked(ctx context.Context, st *store.PlanStep) bool {
	p, err := m.st.GetPlan(ctx, st.PlanID)
	if err != nil {
		return false
	}
	if p.Mode != plan.ModeAutonomous || (p.State != plan.StateApproved && p.State != plan.StatePaused) {
		return false
	}
	live := stepOf(p, st.Idx)
	return live != nil && live.State == plan.StepOpen
}

// parkedStep reports whether a call of a step is parked instead of waited for (D2.8): the step belongs to
// an autonomous plan that still runs, the plan does not cover it, and the call carries no files from the
// sandbox. A step with files is asked synchronously: the gateway cannot repeat it without the running
// sandbox, and the approval has to show the bytes that were read.
func (m *Manager) parkedStep(ctx context.Context, st *store.PlanStep, req platform.Request) bool {
	return st != nil && len(req.Files) == 0 && m.stepIsParked(ctx, st)
}

// parkedAgain answers a call that repeats an already parked step: as long as the approval the step was
// parked with is pending, no second approval is raised and nothing runs — the same answer the agent got
// at the first call, so that it ends its turn (D2.8). waiting is false for every other call.
func (m *Manager) parkedAgain(ctx context.Context, st *store.PlanStep) (platform.Result, bool) {
	if st == nil || st.ParkedApproval == "" {
		return platform.Result{}, false
	}
	ap, err := m.st.GetApproval(ctx, st.ParkedApproval)
	if err != nil || ap.State != store.ApprovalPending {
		return platform.Result{}, false
	}
	return platform.Result{Status: "pending_user", Message: fmt.Sprintf(
		"waiting for the user's approval of step %d; end your turn, you will be woken when the user decides", st.Idx)}, true
}

// parkApproval repeats the wait of a parked step in the background. The approval must already be
// registered with the broker; the step must carry the approval's id.
func (m *Manager) parkApproval(ctx context.Context, chatID, slotID, via string, req platform.Request, st *store.PlanStep, ap store.Approval, w *artifacts.Waiter) {
	bg := context.WithoutCancel(ctx)
	go func() {
		approved, werr := w.Wait(bg, m.parkedTimeout(st))
		m.onParkedDecision(bg, chatID, slotID, via, req, st, ap, approved, werr)
	}()
}

// parkedTimeout is how long a parked approval stays open: as long as its plan may run (D2.8), at least
// the gateway's usual waiting time.
func (m *Manager) parkedTimeout(st *store.PlanStep) time.Duration {
	d := plan.ValidFor
	if p, err := m.st.GetPlan(context.Background(), st.PlanID); err == nil && p.ExpiresAt != nil {
		if left := time.Until(*p.ExpiresAt); left > d {
			d = left
		}
	}
	return d
}

// onParkedDecision runs after the user decided a parked approval. Approval repeats the stored call of
// the step (exactly, with every placeholder resolved) and wakes the agent with the result; rejection
// fails the step and the plan.
func (m *Manager) onParkedDecision(ctx context.Context, chatID, slotID, via string, req platform.Request, st *store.PlanStep, ap store.Approval, approved bool, werr error) {
	final := m.settle(ctx, chatID, ap.ID, approved, werr)
	if final != store.ApprovalApproved {
		why := "rejected by the user"
		if final == store.ApprovalExpired {
			why = "no decision within the waiting time"
		}
		m.failParkedStep(ctx, chatID, st, why)
		return
	}
	// The plan may have ended while the approval waited (cancelled, superseded, expired): then the step
	// is not run any more, and the user's approval has nothing to act on.
	p, err := m.st.GetPlan(ctx, st.PlanID)
	if err != nil || (p.State != plan.StateApproved && p.State != plan.StatePaused) {
		slog.Info("parked approval of a plan that no longer runs", "chat", chatID, "plan", st.PlanID, "step", st.Idx)
		return
	}
	call, args, err := m.parkedCall(ctx, p, *st)
	if err != nil {
		slog.Error("parked step not repeatable", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "error", err)
		m.failParkedStep(ctx, chatID, st, "the step could not be repeated: "+err.Error())
		return
	}
	callCtx := WithParkedApproval(ctx, ap.ID)
	if args != nil {
		// A tool step is repeated as the tool's invocation, so that the marker check finds it again.
		callCtx = platform.WithInvocation(callCtx, st.Tool, args)
	}
	res, cerr := m.PlatformCall(callCtx, chatID, slotID, via, call)
	to, result := plan.StepDone, fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	if cerr != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		to = plan.StepFailed
		if cerr != nil {
			result = cerr.Error()
		}
	}
	m.finishParkedStep(ctx, chatID, st, to, result, cerr == nil)
}

// noteParkedStep tells the agent that its parked step was acted on: the repeat went out (or did not) and
// what came of it; a step that went to "waiting" is reported as such, because the watcher's note about
// the job follows on its own.
func (m *Manager) noteParkedStep(ctx context.Context, chatID string, st *store.PlanStep, to, result string) {
	state := plan.StateApproved
	if fresh, err := m.st.GetPlan(ctx, st.PlanID); err == nil {
		state = fresh.State
	}
	head, body := fmt.Sprintf("Step %d %q ran after your approval: %s.", st.Idx, watch.ClipRunes(oneLine(st.Description), 60), result),
		"The plan continues with its next step."
	switch {
	case to == plan.StepFailed:
		head, body = fmt.Sprintf("Step %d %q was not run: %s.", st.Idx, watch.ClipRunes(oneLine(st.Description), 60), result),
			"The plan ends here. Nothing else was executed."
	case state == plan.StateCompleted:
		body = "That was the last step; the plan is through."
	}
	note := systemNote{Type: store.NotePlanStep, Refs: []string{st.PlanID, fmt.Sprintf("%d", st.Idx)}, Summary: head, Body: body}
	if _, err := m.st.EnqueueSystem(ctx, chatID, note.Type, note.Refs, note.Text()); err != nil {
		slog.Error("note about a parked decision not queued", "chat", chatID, "plan", st.PlanID, "error", err)
		return
	}
	m.publishQueue(ctx, chatID, "queued", nil, "")
	m.publishPlan(ctx, st.PlanID)
	m.wakeForNote(chatID, st.PlanID)
}

// parkedCall rebuilds the call of a step with every placeholder resolved from the plan's earlier steps.
func (m *Manager) parkedCall(ctx context.Context, p store.Plan, st store.PlanStep) (platform.Request, json.RawMessage, error) {
	resolve := func(n int) (string, bool) {
		ref := stepOf(p, n)
		if ref == nil || ref.State != plan.StepDone || ref.CreatedID == "" {
			return "", false
		}
		ok, err := m.st.IsDelegationObject(ctx, p.ChatID, ref.CreatedResource, ref.CreatedID)
		return ref.CreatedID, err == nil && ok
	}
	return plan.StepCall(st, resolve)
}

// failParkedStep ends a parked step as failed and fails its plan (no retry: a step the plan does not
// cover is never claimed, A-E1) — the agent learns it through a note.
func (m *Manager) failParkedStep(ctx context.Context, chatID string, st *store.PlanStep, why string) {
	m.finishParkedStep(ctx, chatID, st, plan.StepFailed, why, false)
}

// finishParkedStep ends a parked step whose decision is in, ends the plan with it and tells the agent.
// The step is unclaimed (a step the plan does not cover never is), so it moves from "open". repeated
// says whether the gateway's own repetition of the step ran: then planFinish has recorded the step
// already (it matched the plan again), and only the note is missing here.
func (m *Manager) finishParkedStep(ctx context.Context, chatID string, st *store.PlanStep, to, result string, repeated bool) {
	ok, err := m.st.FinishPlanStep(ctx, st.PlanID, st.Idx, plan.StepOpen, to, result, "", "")
	if err != nil || !ok {
		if !repeated {
			slog.Error("parked step not finished", "plan", st.PlanID, "step", st.Idx, "error", err)
			return
		}
		// The repetition ran through PlatformCall, so the step is recorded there (planFinish): the plan is
		// already finished or waits for its platform job; only the note about the decision is left.
		if live, gerr := m.st.GetPlan(ctx, st.PlanID); gerr == nil {
			if step := stepOf(live, st.Idx); step != nil && step.State == plan.StepWaiting {
				m.noteParkedStep(ctx, chatID, st, to, "it now waits for its platform job: "+result)
				return
			}
		}
		m.noteParkedStep(ctx, chatID, st, to, result)
		return
	}
	if to == plan.StepFailed {
		if _, err := m.st.FinishPlan(ctx, st.PlanID, plan.StateFailed); err != nil {
			slog.Error("plan not failed", "plan", st.PlanID, "error", err)
		}
	} else if fresh, err := m.st.GetPlan(ctx, st.PlanID); err == nil && allDone(fresh) {
		// The step was the last one: the plan is through with it (like planFinish does for the steps the
		// plan covers).
		if _, err := m.st.FinishPlan(ctx, st.PlanID, plan.StateCompleted); err != nil {
			slog.Error("plan not completed", "plan", st.PlanID, "error", err)
		}
	}
	m.publishPlan(ctx, st.PlanID)
	m.noteParkedStep(ctx, chatID, st, to, result)
}

// StopJob stops the platform job of a plan step or of a single job at the user's request (D2.9, #99).
// The call runs with the user's token through the gateway's platform client and is logged as a user
// call; the agent is not the one asking.
func (m *Manager) StopJob(ctx context.Context, chatID, jobID string) (store.WatchJob, error) {
	job, err := m.st.GetWatchJob(ctx, chatID, jobID)
	if err != nil {
		return store.WatchJob{}, err
	}
	if job.State != store.JobRunning {
		return job, ErrNotRunning
	}
	kind, ok := watchKind(job.Kind)
	if !ok || kind.Stop == nil {
		return job, ErrNotStoppable
	}
	req, ok := kind.Stop(watch.Job{ID: job.ID, ChatID: job.ChatID, Kind: job.Kind, Ref: job.Ref})
	if !ok {
		return job, ErrNotStoppable
	}
	if m.opt.Platform == nil {
		return job, platform.ErrNotConfigured
	}
	m.userActive(chatID)
	// The stop call of a kind may be a virtual path the gateway answers itself (`/_agw/notebook/stop`,
	// own code C-E4): platformDo sends it to the registered doer and everything else to the platform,
	// exactly as m.opt.Platform.Do did before.
	res, err := m.platformDo(ctx, chatID, req)
	detail := req.String() + " (job " + job.ID + ")"
	result := fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	if err != nil {
		result = "error: " + err.Error()
	}
	if eff := delegation.Classify(req).Effect; eff.Valid() {
		// The class in the activity is the same one the agent's own calls of that route carry.
		ctx = store.WithEffect(ctx, string(eff))
	}
	m.LogCallBy(ctx, m.slotOf(chatID), chatID, "user", "platform", detail, result)
	if err != nil {
		return job, err
	}
	if res.Status != "ok" {
		return job, fmt.Errorf("the platform refused the stop: %s", res.Message)
	}
	slog.Info("job stopped by the user", "chat", chatID, "job", job.ID, "kind", job.Kind)
	return job, nil
}

// Jobs lists the observed platform jobs of a chat, newest first (GET /api/chats/{id}/jobs).
func (m *Manager) Jobs(ctx context.Context, chatID string) ([]store.WatchJob, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return nil, err
	}
	return m.st.ChatWatchJobs(ctx, chatID)
}

// StopPlanStep stops the job a waiting step of a plan observes (D2.9): only a step that waits for a job
// whose kind can be stopped, and only for the user — there is no tool for it.
func (m *Manager) StopPlanStep(ctx context.Context, chatID, planID string, idx int) (store.Plan, error) {
	pl, err := m.planOf(ctx, chatID, planID)
	if err != nil {
		return store.Plan{}, err
	}
	st := stepOf(pl, idx)
	if st == nil {
		return pl, store.ErrNotFound
	}
	if st.State != plan.StepWaiting || st.WatchJob == "" {
		return pl, &PlanStateError{State: st.State}
	}
	if _, err := m.StopJob(ctx, chatID, st.WatchJob); err != nil {
		switch {
		case errors.Is(err, ErrNotStoppable), errors.Is(err, ErrNotRunning), errors.Is(err, store.ErrNotFound):
			return pl, &PlanStateError{State: "job " + err.Error()}
		}
		return pl, err
	}
	return m.st.GetPlan(ctx, planID)
}

// ErrNotStoppable: the job's kind has no stop call.
var ErrNotStoppable = errors.New("this job cannot be stopped")

func timePtr(t time.Time) *time.Time { return &t }

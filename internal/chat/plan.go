// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// planNow is the clock of plan validity; tests set it.
var planNow = time.Now

// planClassify gives the effect, the delegation action and the confirm-each trait of a call, the same
// classification that PlatformCall uses (delegation.Classify, issue #63, G1.1).
func planClassify(r platform.Request) (effect, action string, confirmEach bool) {
	a := delegation.Classify(r)
	return string(a.Effect), a.Action, a.ConfirmEach
}

// ProposePlan takes a plan proposal (POST /_agw/plan): one approval covers all steps. An approved
// plan answers with its steps; the steps then run through the next-step match (planMatch). An autonomous
// plan (A-E1) additionally carries a budget and a grant derived from its steps that bounds every call of
// the chat while it runs.
func (m *Manager) ProposePlan(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	if m.inTrash(ctx, chatID) {
		return platform.Result{Status: "error", Message: platform.RefusedPrefix + trashRefusal}, nil
	}
	lim := plan.Limits{MaxDuration: m.opt.PlanMaxDuration, MaxLLMCost: m.opt.PlanMaxLLMCost}
	// Every problem of the proposal comes back in one answer (issue #218): what ParseProposal found and what
	// Build finds in the steps that did read.
	p, perr := plan.ParseProposal(req.Body, lim)
	if len(p.Steps) == 0 {
		msg := "plan is not valid"
		if perr != nil {
			msg = perr.Error()
		}
		return platform.Result{Status: "error", Message: msg}, nil
	}
	steps, berr := plan.Build(p, planClassify)
	if err := plan.Join(perr, berr); err != nil {
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	// #219: trace the image of every deployment start before the autonomous checks, so that the grant and the
	// compute count see the final Covered of the steps.
	rl := &readLog{}
	replaced := m.replacedPlanOf(ctx, chatID, p.Replaces)
	plan.ImageOrigins(steps, replaced, m.templateOK(ctx, m.loggedSummaryReader(chatID, rl)))
	var err error
	var (
		mode   string
		budget store.PlanBudget
		grant  json.RawMessage
		auto   *plan.Autonomy
	)
	if p.Autonomous() {
		mode = plan.ModeAutonomous
		del, gerr := plan.Grant(steps, plan.SampleStep)
		if gerr != nil {
			return platform.Result{Status: "error", Message: gerr.Error()}, nil
		}
		if err := m.checkAutonomous(ctx, chatID, steps, p); err != nil {
			return platform.Result{Status: "error", Message: err.Error()}, nil
		}
		if grant, err = json.Marshal(del); err != nil {
			return platform.Result{}, err
		}
		budget = store.PlanBudget{MaxDurationS: p.Budget.MaxDurationMinutes * 60,
			MaxLLMCost: p.Budget.MaxLLMCostUSD, MaxAutoTurns: p.Budget.MaxAutoTurns}
		need := plan.ComputeSteps(steps)
		left := m.computeLeft(ctx, chatID)
		cost, _ := m.replacedCost(ctx, chatID, p.Replaces)
		auto = &plan.Autonomy{Budget: *p.Budget, Rights: del.SummaryLine(), ComputeNeeded: need,
			ComputeLeft: max(0, left-need), Replaces: p.Replaces, ReplacedCostUSD: cost}
		// D3.3: an autonomous plan must not run into the end of the login session that carries its calls.
		// That end is fixed the moment the session was created, so it is read once, here: the approval's
		// waiting time eats into it, and the preview names it (Autonomy.EndsAt).
		if ends, ok := m.planSessionEnd(ctx, chatID); ok {
			if !ends.After(planNow()) {
				return platform.Result{Status: "error", Message: "the login session ends within the hour; an autonomous plan would not get anywhere. Log in to the platform again and propose it once more"}, nil
			}
			if own := planNow().Add(time.Duration(p.Budget.MaxDurationMinutes) * time.Minute); ends.Before(own) {
				auto.EndsAt = timePtr(ends)
			}
		}
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return platform.Result{}, err
	}
	// What each step does is written by the gateway from the stored step and platform state read now (issue #72);
	// the agent's description stays a separate note.
	calls := make([]platform.SummaryCall, len(steps))
	for i := range steps {
		calls[i] = plan.SummaryCall(steps[i])
	}
	for i, s := range platform.SummarizePlan(ctx, m.loggedSummaryReader(chatID, rl), calls) {
		steps[i].Summary = platform.VisibleControls(s)
	}
	preview := platform.VisibleControls(plan.Preview(p.Title, steps, auto))
	if n := len([]rune(preview)); n > maxPlatformPreview {
		return platform.Result{Status: "error", Message: fmt.Sprintf("plan too large for an approval (%d characters, at most %d); split it", n, maxPlatformPreview)}, nil
	}
	session := store.SessionFrom(ctx)
	pl, err := m.st.CreatePlan(ctx, store.Plan{ChatID: chatID, Title: p.Title, Session: session, ToolCallID: store.ToolCallFrom(ctx), Via: via, Steps: steps,
		Mode: mode, Budget: budget, Grant: grant, Replaces: p.Replaces})
	if err != nil {
		return platform.Result{}, err
	}
	// The id is assigned here and the recheck is registered before the insert: the pending plan approval is
	// visible to the API (and to the user) as soon as the row exists, and a click that arrives before the
	// registration would skip the re-read and approve the summaries as proposed (issue #154).
	apID := uuid.NewString()
	defer m.registerRecheck(apID, m.planRecheck(chatID, apID, p.Title, steps, calls, auto))()
	d := &planDecision{apply: func(bg context.Context, final string) (bool, error) {
		if final != store.ApprovalApproved {
			state := plan.StateRejected
			if final == store.ApprovalExpired {
				state = plan.StateExpired
			}
			if _, err := m.st.DecidePlan(bg, pl.ID, state, nil); err != nil {
				slog.Error("plan decision not recorded", "plan", pl.ID, "error", err)
			}
			m.publishPlan(bg, pl.ID)
			return false, nil
		}
		expires := planNow().Add(plan.ValidFor)
		if p.Autonomous() {
			// The plan's own duration bounds it (A-E1, D1.8); the validity of an interactive plan does not apply.
			expires = planNow().Add(time.Duration(p.Budget.MaxDurationMinutes) * time.Minute)
			if auto.EndsAt != nil {
				// D3.3: the login session of the owner ends earlier than the plan's own duration.
				expires = *auto.EndsAt
			}
		}
		ok, err := m.st.DecidePlan(bg, pl.ID, plan.StateApproved, &expires)
		if err != nil || !ok {
			return ok, err
		}
		if err := m.st.SupersedePlans(bg, chatID, session, pl.ID); err != nil {
			slog.Error("older plans not superseded", "chat", chatID, "plan", pl.ID, "error", err)
		}
		if p.Autonomous() && p.Replaces != "" {
			// A changed plan replaces the running one it names, independent of the session (A-E1, D1.12).
			if _, err := m.st.SupersedePlan(bg, p.Replaces); err != nil {
				slog.Error("replaced plan not superseded", "chat", chatID, "plan", p.Replaces, "error", err)
			}
		}
		m.publishPlan(bg, pl.ID)
		return true, nil
	}}
	defer m.registerPlanDecision(apID, d)()
	// The reads of the summaries are one activity entry of the system (issue L2/#119), and while the plan waits,
	// approving re-reads them first (DL2.1). It is written before the approval row exists: an approval that the
	// API shows must never be visible without the entry that explains its summaries (issue #157).
	m.logSummaryReads(ctx, chatID, apID, "proposed", rl)
	ap, err := m.st.CreateApproval(ctx, store.Approval{ID: apID, ChatID: chatID, Kind: "plan", Via: via, Name: plan.ApprovalName(len(steps)), Size: int64(len(steps)),
		ContentType: "application/json", PendingKey: "-", Preview: preview, Summary: plan.ApprovalSummary(steps), Session: session, ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		return platform.Result{}, err
	}
	if err := m.st.SetPlanApproval(ctx, pl.ID, ap.ID); err != nil {
		return platform.Result{}, err
	}
	w := m.broker.Register(ap.ID)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishPlan(ctx, pl.ID)
	m.publishChat(ctx, chatID)
	slog.Info("plan proposed", "chat", chatID, "approval", ap.ID, "plan", pl.ID, "steps", len(steps), "via", via)
	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("tool", "")
	}
	m.publishChat(bg, chatID)
	// Normally Decide has recorded the outcome already and this returns the stored result (issue #166).
	ok, err := d.decide(bg, final)
	if final != store.ApprovalApproved {
		msg := "plan not approved by the user; nothing executed. Ask the user what to change and propose a new plan"
		if final == store.ApprovalExpired {
			msg = "no decision on the plan within the waiting time; nothing executed"
		}
		return platform.Result{Status: "rejected", Message: msg}, nil
	}
	if err != nil {
		return platform.Result{}, err
	}
	if !ok {
		return platform.Result{Status: "error", Message: "plan was decided elsewhere; nothing executed"}, nil
	}
	stored, err := m.st.GetPlan(bg, pl.ID)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: plan.ApprovedText(stored)}, nil
}

// summaryReader reads the platform as the user for the summaries of approvals (issue #72): only reading GETs, straight
// through the platform client. These reads belong to the gateway, not to the agent; they are logged as one
// summary_read entry of the orchestrator (issue L2/#119), never as a call of the agent. Use loggedSummaryReader
// wherever a summary is written or re-read.
func (m *Manager) summaryReader(chatID string) platform.Doer {
	if m.opt.Platform == nil {
		return nil
	}
	return func(ctx context.Context, r platform.Request) (platform.Result, error) {
		if r.Method != http.MethodGet || r.Writes() {
			return platform.Result{Status: "error", Message: "not a reading call"}, nil
		}
		// A path the gateway answers itself (G1.5) is read from the gateway, not from the platform: an
		// approval names the last check of a model (R1.14) no differently from a dataset's name.
		if d, ok := m.virtualFor(r.Path); ok {
			return d.Do(ctx, chatID, r)
		}
		return m.opt.Platform.Do(ctx, chatID, r)
	}
}

// planMatch returns the next step of the session's approved plan if the call is exactly that step.
// A covered step is claimed (open to running) and the caller runs it without a new approval; an
// irreversible step is returned unclaimed, so the call still asks the user. nil: no match.
func (m *Manager) planMatch(ctx context.Context, chatID string, req platform.Request) *store.PlanStep {
	st, _, _ := m.planNext(ctx, chatID, req)
	return st
}

// planNext is planMatch with what a refusal needs: next is the number of the plan's next open step and differs
// names the first part of the call that is not the step ("files" when only the uploads differ, issue #172);
// both are empty/zero without an active plan or on a match.
func (m *Manager) planNext(ctx context.Context, chatID string, req platform.Request) (*store.PlanStep, int, string) {
	p, err := m.st.ActivePlan(ctx, chatID, store.SessionFrom(ctx))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("active plan not readable", "chat", chatID, "error", err)
		}
		return nil, 0, ""
	}
	if p.ExpiresAt != nil && !planNow().Before(*p.ExpiresAt) {
		if ok, err := m.st.SetPlanState(ctx, p.ID, plan.StateApproved, plan.StateExpired); err == nil && ok {
			m.publishPlan(ctx, p.ID)
		}
		return nil, 0, ""
	}
	var cur *store.PlanStep
	for i := range p.Steps {
		if p.Steps[i].State != plan.StepDone {
			cur = &p.Steps[i]
			break
		}
	}
	if cur == nil {
		return nil, 0, ""
	}
	if cur.State != plan.StepOpen {
		slog.Info("call while the plan's next step is not open", "chat", chatID, "plan", p.ID, "step", cur.Idx, "state", cur.State)
		return nil, 0, ""
	}
	resolve := m.stepResolve(ctx, chatID, p.Steps)
	var ok bool
	var why string
	if plan.IsToolStep(*cur) {
		// A tool step matches the tool invocation, not the HTTP request: only the socket sets it, so a raw
		// request with the same target is not covered (issue #70).
		inv, has := platform.InvocationFrom(ctx)
		if !has {
			ok, why = false, "no tool invocation"
		} else {
			ok, why = plan.MatchesTool(*cur, inv.Tool, inv.Args, req, resolve)
		}
	} else {
		ok, why = plan.Matches(*cur, req, resolve)
	}
	if !ok {
		slog.Info("call is not the plan's next step", "chat", chatID, "plan", p.ID, "step", cur.Idx, "differs_in", why)
		return nil, cur.Idx, why
	}
	if !cur.Covered {
		return cur, cur.Idx, ""
	}
	claimed, err := m.st.ClaimPlanStep(ctx, p.ID, cur.Idx)
	if err != nil || !claimed {
		return nil, cur.Idx, ""
	}
	cur.State = plan.StepRunning
	cur.Attempts++
	m.publishPlan(context.WithoutCancel(ctx), p.ID)
	return cur, cur.Idx, ""
}

// planFinish records the outcome of a step. A created object enters the provenance register, so a
// later step can name it. A call that started a platform job the gateway observes (A-E2) does not
// finish the step: it waits until that job ends (StepWaiting). A failed step fails the plan; when every
// step is done the plan completes. A call that never went out because the user's login was gone (A-E3,
// D3.5) is not a failure at all: the step keeps its claim back and stays open.
//
// It returns the id of the platform job the step now waits for, "" for every other outcome: the caller
// tells the agent, so that it ends its turn instead of polling the job itself (A-E2, D2.1).
func (m *Manager) planFinish(ctx context.Context, chatID string, st *store.PlanStep, access delegation.Access, req platform.Request, res platform.Result, callErr error) string {
	if st == nil {
		return ""
	}
	bg := context.WithoutCancel(ctx)
	from := plan.StepOpen
	if st.Covered {
		from = plan.StepRunning
	}
	if loginGone(callErr) {
		// D3.5: the user's login was gone, so the call never went out. That is no failure of the step and no
		// reason to fail the plan: a claimed step goes back to open without counting against max_attempts (its
		// claim is given back), an unclaimed one stays open as it was. The caller pauses the plan.
		if from == plan.StepRunning {
			ok, err := m.st.ReleasePlanStep(bg, st.PlanID, st.Idx, callErr.Error())
			if err != nil || !ok {
				slog.Error("claimed plan step not released after a lost login", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "ok", ok, "error", err)
			}
		}
		m.publishPlan(bg, st.PlanID)
		return ""
	}
	to, resource, id := plan.StepFailed, "", ""
	result := fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	if callErr != nil {
		result = callErr.Error()
	} else if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		// A failed call keeps the platform's own words, so the plan view can say why (issue #221).
		if line := failureLine(res); line != "" {
			result += ": " + line
		}
	} else if res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300 {
		to = plan.StepDone
		if r, i, ok := delegation.Created(access, res); ok {
			resource, id = r, i
			if err := m.st.AddDelegationObject(bg, chatID, r, i); err != nil {
				slog.Error("provenance of a plan step not recorded", "chat", chatID, "object", r+" "+i, "error", err)
			}
		}
		// The step started a platform job (A-E2, D2.2): the job is observed from here on, and the step
		// waits for its end. The object the call created is already in the register, so a later step can
		// name it while the job runs.
		if kind, ref, watching := watch.For(access, req, res); watching {
			if jobID := m.startWatching(bg, chatID, st, from, kind, ref, result, resource, id); jobID != "" {
				return jobID
			}
		}
	}
	ok, err := m.st.FinishPlanStep(bg, st.PlanID, st.Idx, from, to, result, resource, id)
	if err != nil || !ok {
		slog.Error("plan step not finished", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "error", err)
		return ""
	}
	if to == plan.StepFailed {
		// An autonomous step that was claimed and has attempts left goes back to open and the plan stays
		// approved (A-E1, D1.7); otherwise the plan fails. A step the plan does not cover (irreversible,
		// confirm each) is never claimed, so it never retries — it asks for every call anyway.
		retried := false
		if st.Covered {
			var rerr error
			retried, rerr = m.st.RetryPlanStep(bg, st.PlanID, st.Idx)
			if rerr != nil {
				slog.Error("plan step not retried", "plan", st.PlanID, "step", st.Idx, "error", rerr)
			}
		}
		if !retried {
			if _, err := m.st.FinishPlan(bg, st.PlanID, plan.StateFailed); err != nil {
				slog.Error("plan not failed", "plan", st.PlanID, "error", err)
			}
		}
	} else if p, err := m.st.GetPlan(bg, st.PlanID); err == nil && allDone(p) {
		if _, err := m.st.FinishPlan(bg, st.PlanID, plan.StateCompleted); err != nil {
			slog.Error("plan not completed", "plan", st.PlanID, "error", err)
		}
	}
	m.publishPlan(bg, st.PlanID)
	return ""
}

func allDone(p store.Plan) bool {
	for _, s := range p.Steps {
		if s.State != plan.StepDone {
			return false
		}
	}
	return true
}

// publishPlan sends the plan's current state to the subscribers of its chat and, for an autonomous plan,
// the alert of the state it just reached (A-E4, D4.3). It is the one way a plan's state change is
// announced, so the cached active plan of the chat and the alert's mark are dropped here (A-E1).
func (m *Manager) publishPlan(ctx context.Context, planID string) {
	p, err := m.st.GetPlan(ctx, planID)
	if err != nil {
		slog.Error("plan event not published", "plan", planID, "error", err)
		return
	}
	m.invalidatePlanCache(p.ChatID)
	m.fillPlanUsage(ctx, &p)
	m.publish(p.ChatID, Event{Kind: "plan", Data: redactPlanText(p)})
	m.publishPlanAlerts(ctx, p)
}

// redactPlanText keeps tokens out of the progress texts of a plan's steps on the way to the UI (issue #141):
// the messages come from the platform, and rows stored before the gateway redacted them still carry one.
func redactPlanText(p store.Plan) store.Plan {
	for i := range p.Steps {
		p.Steps[i].ProgressMsg = platform.RedactText(p.Steps[i].ProgressMsg)
		p.Steps[i].LogTail = watch.Tail(platform.RedactText(p.Steps[i].LogTail))
		p.Steps[i].Result = platform.RedactText(p.Steps[i].Result)
	}
	return plan.WithUploads(p)
}

// PlanStepUploadEntries returns the list of every file of an upload step of a plan of the chat (issue #172): the
// card fetches it when the user opens the list. The plan event carries only the summary and the manifest hash.
func (m *Manager) PlanStepUploadEntries(ctx context.Context, chatID, planID string, idx int) ([]platform.UploadEntry, error) {
	pl, err := m.planOf(ctx, chatID, planID)
	if err != nil {
		return nil, err
	}
	st := stepOf(pl, idx)
	if st == nil {
		return nil, store.ErrNotFound
	}
	man, ok := plan.StepManifest(*st)
	if !ok {
		return nil, store.ErrNotFound
	}
	return man.Entries, nil
}

// fillPlanUsage writes what the plan has used of its budget onto it (A-E4, D4.5). An unreadable usage
// leaves the field empty instead of failing the plan: the card then shows no budget line.
func (m *Manager) fillPlanUsage(ctx context.Context, p *store.Plan) {
	if p.Mode != plan.ModeAutonomous {
		return
	}
	u, err := m.st.PlanUsageOf(ctx, *p)
	if err != nil {
		slog.Error("plan usage not readable", "plan", p.ID, "error", err)
		return
	}
	u.ComputeMax = m.computeMax()
	p.Usage = &u
}

// Plans lists the plans of a chat, oldest first, with the usage of the autonomous ones (A-E4, D4.5).
func (m *Manager) Plans(ctx context.Context, chatID string) ([]store.Plan, error) {
	list, err := m.st.ListPlans(ctx, chatID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		m.fillPlanUsage(ctx, &list[i])
	}
	return list, nil
}

// PlansAcross lists one page of the plans of the user's chats (GET /api/plans, issue #223): running ones first,
// each with its steps, the chat title and the usage of the autonomous ones. total counts all plans matching the
// filter, running the approved and paused plans of the owner whatever the filter.
func (m *Manager) PlansAcross(ctx context.Context, f store.PlanListFilter) (items []store.PlanListItem, total, running int, err error) {
	items, total, err = m.st.ListPlansAcross(ctx, f)
	if err != nil {
		return nil, 0, 0, err
	}
	for i := range items {
		m.fillPlanUsage(ctx, &items[i].Plan)
	}
	running, err = m.st.RunningPlanCount(ctx, f.Owner)
	if err != nil {
		return nil, 0, 0, err
	}
	return items, total, running, nil
}

// ChatTitle returns the title of a chat and whether it is in the trash (for GET /api/plans/events).
func (m *Manager) ChatTitle(ctx context.Context, id string) (title string, trashed bool, err error) {
	c, err := m.st.GetChat(ctx, id)
	if err != nil {
		return "", false, err
	}
	return c.Title, c.TrashedAt != nil, nil
}

// --- Autonomous plans: proposal checks and the user's controls (A-E1) ---

// computeLeft is how many compute calls the chat has left (AGW_COMPUTE_MAX_PER_CHAT, never negative).
func (m *Manager) computeLeft(ctx context.Context, chatID string) int {
	n, err := m.st.ComputeCalls(ctx, chatID)
	if err != nil {
		return 0
	}
	if left := m.computeMax() - n; left > 0 {
		return left
	}
	return 0
}

// checkAutonomous validates an autonomous proposal before it is stored (A-E1): every step lies inside the
// chat's delegated rights (S3), the compute quota suffices (D1.7) and "replaces" names a running plan of
// this chat (D1.12).
func (m *Manager) checkAutonomous(ctx context.Context, chatID string, steps []store.PlanStep, p plan.Proposal) error {
	chatDel, err := m.delegationOf(ctx, chatID)
	if err != nil {
		return errors.New("the delegated rights of this chat are unreadable")
	}
	if chatDel != nil && chatDel.Enforcing() {
		own := func(res, id string) bool {
			// A placeholder stands for an object this plan creates, so a chat right on "own" objects covers
			// it (A-E1); the provenance register cannot know it yet.
			if plan.IsPlaceholderID(id) {
				return true
			}
			ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
			return ok
		}
		for _, st := range steps {
			sample, serr := plan.SampleStep(st)
			if serr != nil {
				return fmt.Errorf("step %d: %v", st.Idx, serr)
			}
			if dec := chatDel.Check(delegation.Classify(sample), time.Now(), own); !dec.Allowed {
				return fmt.Errorf("step %d: outside the chat's delegated rights: %s", st.Idx, dec.Reason)
			}
		}
	}
	need := plan.ComputeSteps(steps)
	if left := m.computeLeft(ctx, chatID); need > left {
		return fmt.Errorf("plan needs %d compute calls, this chat has %d left (AGW_COMPUTE_MAX_PER_CHAT)", need, left)
	}
	if p.Replaces != "" {
		rp, rerr := m.st.GetPlan(ctx, p.Replaces)
		if rerr != nil || rp.ChatID != chatID {
			return fmt.Errorf("replaces: no running plan %s in this chat", p.Replaces)
		}
		if rp.State != plan.StateApproved && rp.State != plan.StatePaused {
			return fmt.Errorf("replaces: plan %s is %s, not running", p.Replaces, rp.State)
		}
	}
	return nil
}

// replacedCost is what the model calls of a plan cost so far; zero without a plan or before its approval.
func (m *Manager) replacedCost(ctx context.Context, chatID, planID string) (float64, error) {
	if planID == "" {
		return 0, nil
	}
	rp, err := m.st.GetPlan(ctx, planID)
	if err != nil || rp.DecidedAt == nil {
		return 0, nil
	}
	return m.st.PlanLLMCost(ctx, chatID, *rp.DecidedAt)
}

// autonomyOf rebuilds the preview's autonomy block from a stored plan (A-E1); nil for an interactive plan.
// ComputeLeft is -1 (unknown) because the chat's remaining quota is not read here.
func autonomyOf(p store.Plan) *plan.Autonomy {
	if p.Mode != plan.ModeAutonomous {
		return nil
	}
	a := &plan.Autonomy{
		Budget:   plan.Budget{MaxDurationMinutes: p.Budget.MaxDurationS / 60, MaxLLMCostUSD: p.Budget.MaxLLMCost, MaxAutoTurns: p.Budget.MaxAutoTurns},
		Replaces: p.Replaces, ComputeLeft: -1, ComputeNeeded: plan.ComputeSteps(p.Steps),
	}
	if g, err := delegation.Parse(p.Grant); err == nil {
		a.Rights = g.SummaryLine()
	}
	return a
}

// PlanStateError reports an attempt to move a plan that is not in the required state; the API answers 409.
type PlanStateError struct {
	State string
	// Reason replaces the "plan is <state>" text where the plan's state is not what is wrong: the step
	// waits, but its job cannot be stopped. It is the message the user sees.
	Reason string
}

func (e *PlanStateError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "plan is " + e.State
}

// planOf loads a plan and checks that it belongs to the chat; a foreign or unknown plan is ErrNotFound.
func (m *Manager) planOf(ctx context.Context, chatID, planID string) (store.Plan, error) {
	p, err := m.st.GetPlan(ctx, planID)
	if err != nil {
		return store.Plan{}, err
	}
	if p.ChatID != chatID {
		return store.Plan{}, store.ErrNotFound
	}
	return p, nil
}

// chatRunning reports whether a turn is going on in the chat.
func (m *Manager) chatRunning(chatID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.live[chatID]
	return l != nil && l.running
}

// pausePlan pauses an approved plan in the store and publishes it, without touching the chat (Abort uses
// this too, so the two do not call each other).
func (m *Manager) pausePlan(ctx context.Context, chatID, planID, reason string) (store.Plan, error) {
	p, err := m.planOf(ctx, chatID, planID)
	if err != nil {
		return store.Plan{}, err
	}
	if p.State != plan.StateApproved {
		return p, &PlanStateError{State: p.State}
	}
	ok, err := m.st.PausePlan(ctx, planID, reason)
	if err != nil {
		return p, err
	}
	if !ok {
		fresh, gerr := m.st.GetPlan(ctx, planID)
		if gerr != nil {
			return p, gerr
		}
		return fresh, &PlanStateError{State: fresh.State}
	}
	m.publishPlan(ctx, planID)
	return m.st.GetPlan(ctx, planID)
}

// PausePlan pauses an autonomous plan (POST …/pause) and stops a running chat, so the agent halts until
// the user resumes it (A-E1, D1.10).
func (m *Manager) PausePlan(ctx context.Context, chatID, planID, reason string) (store.Plan, error) {
	p, err := m.pausePlan(ctx, chatID, planID, reason)
	if err != nil {
		return p, err
	}
	if m.chatRunning(chatID) {
		if _, aerr := m.Abort(ctx, chatID); aerr != nil {
			return p, aerr
		}
	}
	return p, nil
}

// ResumePlan moves a paused plan back to approved (POST …/resume) and queues a note so the agent continues.
// Only the user's API reaches this; there is no tool for it, so the agent cannot resume itself (A-E1).
func (m *Manager) ResumePlan(ctx context.Context, chatID, planID string) (store.Plan, error) {
	p, err := m.planOf(ctx, chatID, planID)
	if err != nil {
		return store.Plan{}, err
	}
	if p.State != plan.StatePaused {
		return p, &PlanStateError{State: p.State}
	}
	ok, err := m.st.ResumePlan(ctx, planID)
	if err != nil {
		return p, err
	}
	if !ok {
		fresh, gerr := m.st.GetPlan(ctx, planID)
		if gerr != nil {
			return p, gerr
		}
		return fresh, &PlanStateError{State: fresh.State}
	}
	m.resetFailedCalls(chatID) // the user resumes: the counters of failed calls and a stop for them end (#217)
	if p.Mode == plan.ModeAutonomous {
		// D3.4: a step a restart failed stays failed while the plan is paused, and a failed step blocks the
		// plan (the agent may only run its next open step). The user's resume is exactly the repeat the
		// message of that failure asks for — they checked on the platform whether the call arrived.
		if n, rerr := m.st.ReopenFailedPlanSteps(ctx, planID); rerr != nil {
			slog.Error("failed plan steps not reopened", "chat", chatID, "plan", planID, "error", rerr)
		} else if n > 0 {
			slog.Info("failed plan steps reopened by the user's resume", "chat", chatID, "plan", planID, "steps", n)
		}
	}
	m.publishPlan(ctx, planID)
	note := fmt.Sprintf("The user resumed plan %q. Continue with step %d.", planID, nextStepIdx(p))
	if _, err := m.st.EnqueueSystem(ctx, chatID, store.NotePlan, []string{planID}, note); err != nil {
		slog.Error("resume note not queued", "chat", chatID, "plan", planID, "error", err)
	} else {
		m.publishQueue(ctx, chatID, "queued", nil, "")
	}
	return m.st.GetPlan(ctx, planID)
}

// CancelPlan ends an approved or paused plan (POST …/cancel), stops a running chat and queues a note; the
// plan's open steps become skipped, its completed steps stay (A-E1).
func (m *Manager) CancelPlan(ctx context.Context, chatID, planID string) (store.Plan, error) {
	p, err := m.planOf(ctx, chatID, planID)
	if err != nil {
		return store.Plan{}, err
	}
	if p.State != plan.StateApproved && p.State != plan.StatePaused {
		return p, &PlanStateError{State: p.State}
	}
	ok, err := m.st.CancelPlan(ctx, planID)
	if err != nil {
		return p, err
	}
	if !ok {
		fresh, gerr := m.st.GetPlan(ctx, planID)
		if gerr != nil {
			return p, gerr
		}
		return fresh, &PlanStateError{State: fresh.State}
	}
	m.publishPlan(ctx, planID)
	if m.chatRunning(chatID) {
		if _, aerr := m.Abort(ctx, chatID); aerr != nil {
			return p, aerr
		}
	}
	note := fmt.Sprintf("The user cancelled plan %q. Steps %s were not run. Do not continue it.", planID, undoneSteps(p))
	if _, err := m.st.EnqueueSystem(ctx, chatID, store.NotePlan, []string{planID}, note); err != nil {
		slog.Error("cancel note not queued", "chat", chatID, "plan", planID, "error", err)
	} else {
		m.publishQueue(ctx, chatID, "queued", nil, "")
	}
	return m.st.GetPlan(ctx, planID)
}

// nextStepIdx is the first step of the plan that is not done (1 if none is open).
func nextStepIdx(p store.Plan) int {
	for _, st := range p.Steps {
		if st.State != plan.StepDone {
			return st.Idx
		}
	}
	return 0
}

// undoneSteps lists the steps of a plan that are not done, as "2, 3".
func undoneSteps(p store.Plan) string {
	var idx []string
	for _, st := range p.Steps {
		if st.State != plan.StepDone {
			idx = append(idx, strconv.Itoa(st.Idx))
		}
	}
	if len(idx) == 0 {
		return "none"
	}
	return strings.Join(idx, ", ")
}

// stepResolve gives the id that placeholder {{stepN.id}} stands for: the object step N created, once the
// step is done and the object is one this chat created (the register of delegation objects).
func (m *Manager) stepResolve(ctx context.Context, chatID string, steps []store.PlanStep) plan.Resolve {
	return func(n int) (string, bool) {
		var ref *store.PlanStep
		for i := range steps {
			if steps[i].Idx == n {
				ref = &steps[i]
				break
			}
		}
		if ref == nil || ref.State != plan.StepDone || ref.CreatedID == "" {
			return "", false
		}
		ok, err := m.st.IsDelegationObject(ctx, chatID, ref.CreatedResource, ref.CreatedID)
		return ref.CreatedID, err == nil && ok
	}
}

// failureLine is the platform's reason for a failed call as one short line: the gateway's message, else the
// detail of the JSON body (text, or the first msg of a list as FastAPI sends validation errors). The bare
// "platform responds <code>" of the client says less than the body and gives way to it. Tokens are redacted and
// the line is clipped. Empty when the platform said nothing usable.
func failureLine(res platform.Result) string {
	line := strings.TrimSpace(res.Message)
	generic := strings.HasPrefix(line, "platform responds ")
	if (line == "" || generic) && res.Body != "" {
		var doc struct {
			Detail json.RawMessage `json:"detail"`
		}
		if json.Unmarshal([]byte(res.Body), &doc) == nil && len(doc.Detail) > 0 {
			var text string
			var list []struct {
				Msg string `json:"msg"`
			}
			if json.Unmarshal(doc.Detail, &text) == nil {
				line = text
			} else if json.Unmarshal(doc.Detail, &list) == nil {
				for _, e := range list {
					if e.Msg != "" {
						line = e.Msg
						break
					}
				}
			}
		}
	}
	if line == "" {
		return ""
	}
	line = strings.Join(strings.Fields(line), " ")
	return watch.ClipRunes(platform.RedactText(line), 200)
}

// replacedPlanOf returns the plan that "replaces" names, for the image origin of a deployment start (#219); nil
// without "replaces", on a read error and for a plan of another chat.
func (m *Manager) replacedPlanOf(ctx context.Context, chatID, planID string) *store.Plan {
	if planID == "" {
		return nil
	}
	rp, err := m.st.GetPlan(ctx, planID)
	if err != nil || rp.ChatID != chatID {
		return nil
	}
	return &rp
}

// templateOK answers whether an installed template comes from the one allowed repository (#219). It reads
// /inference-container-templates at most once, at the first question; without a reader or after a read error
// every answer is false.
func (m *Manager) templateOK(ctx context.Context, read platform.Doer) plan.TemplateOK {
	var (
		loaded bool
		ok     map[string]bool
	)
	return func(id string) bool {
		if !loaded {
			loaded = true
			ok = map[string]bool{}
			if read == nil {
				return false
			}
			res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/inference-container-templates", Query: map[string]string{"limit": "1000"}})
			if err != nil || res.Status != "ok" || res.HTTPStatus >= 400 {
				return false
			}
			var list []struct {
				ID     json.Number `json:"id"`
				GitURL string      `json:"git_url"`
			}
			if json.Unmarshal([]byte(res.Body), &list) != nil || res.Truncated {
				return false
			}
			for _, t := range list {
				if t.GitURL == platform.AllowedTemplateGitURL {
					ok[t.ID.String()] = true
				}
			}
		}
		return ok[id]
	}
}

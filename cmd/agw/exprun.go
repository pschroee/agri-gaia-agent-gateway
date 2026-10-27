// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

// The experiment runner (X1.4): one process starts every run of a plan or a cell, one after the
// other, each in a fresh experiment chat. It keeps its state in the gateway's exp_runs table, so a
// restart continues where the last process stopped (X1.5). It talks to the gateway over /api/exp/*
// with the experiment key; the platform token of the account never leaves the gateway.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

const expStarted = "started"

// expAPI is the part of the gateway the runner uses. The real implementation is *agwclient.Client;
// the tests replace it.
type expAPI interface {
	Models(ctx context.Context) ([]agwclient.Model, error)
	CreateChat(ctx context.Context, req agwclient.CreateChatRequest) (agwclient.Chat, error)
	Chat(ctx context.Context, id string) (agwclient.ChatDetail, error)
	Jobs(ctx context.Context, id string) ([]agwclient.WatchJob, error)
	ExpEnvironment(ctx context.Context) (agwclient.ExpEnvironment, error)
	ExpStartRun(ctx context.Context, run agwclient.ExpRun) error
	ExpSetRunChat(ctx context.Context, id, chatID string) error
	ExpEndRun(ctx context.Context, end agwclient.ExpRunEnd) error
	ExpSupersedeRun(ctx context.Context, id, by string) error
	ExpRuns(ctx context.Context, plan, cell string) ([]agwclient.ExpRun, error)
	ExpUsage(ctx context.Context, plan string) (agwclient.ExpUsage, error)
	ExpObjects(ctx context.Context, chatID string) ([]agwclient.ExpObject, error)
	ExpCall(ctx context.Context, req agwclient.ExpCallRequest) (agwclient.ExpCallResult, error)
	ExpAbortChat(ctx context.Context, chatID string) error
	ExpSeed(ctx context.Context, plan, cell, run string, specs []exp.FixtureSpec) (agwclient.ExpFixtureAnswer, int, error)
	ExpUnseed(ctx context.Context, plan, cell string) (agwclient.ExpFixtureAnswer, error)
}

// expRunOptions are the switches of agw exp run.
type expRunOptions struct {
	PlanPath string
	Cells    []string
	K        int
	Seed     int64
	SeedSet  bool
	DryRun   bool

	// Poll, StartGrace and Heartbeat are the waiting intervals (tests shorten them).
	Poll time.Duration
	// LimitGrace is how long a limit may stay exceeded before the runner reports that the gateway did not enforce it (#213).
	LimitGrace time.Duration
	StartGrace time.Duration
	Heartbeat  time.Duration
	Now        func() time.Time
}

// expPlannedRun is one run of the order the runner works through (X1.6).
type expPlannedRun struct {
	Key      string
	Plan     string
	Cell     string
	Task     string
	Rep      int
	Seed     int64
	Position int
}

type expRunner struct {
	api  expAPI
	out  io.Writer
	errw io.Writer

	plan      *exp.Plan
	tasks     []exp.Task
	manifests []exp.Manifest
	specsDir  string // the material directory, for the content files of the fixtures
	planName  string
	planSHA   string
	lock      *exp.Lock // set for a main plan (X8.5); nil for a pilot
	order     []expPlannedRun

	taskByID map[string]exp.Task
	cellByID map[string]exp.Cell
	models   map[string]string // role → model id

	k          int
	seed       int64
	dryRun     bool
	poll       time.Duration
	limitGrace time.Duration
	startGrace time.Duration
	heartbeat  time.Duration
	now        func() time.Time

	noSlotWait    time.Duration
	noSlotRetries int
}

// runExpPlan loads the material, builds the order and runs it.
func runExpPlan(ctx context.Context, api expAPI, opts expRunOptions, out, errw io.Writer) error {
	r, err := newExpRunner(api, opts, out, errw)
	if err != nil {
		return err
	}
	return r.run(ctx)
}

func newExpRunner(api expAPI, opts expRunOptions, out, errw io.Writer) (*expRunner, error) {
	if opts.PlanPath == "" {
		return nil, usagef("--plan is required")
	}
	m, err := loadMaterial(opts.PlanPath)
	if err != nil {
		return nil, err
	}
	p, tasks, manifests, dir := m.Plan, m.Tasks, m.Manifests, m.Dir
	problems := exp.Validate(p, tasks, manifests)
	if len(problems) > 0 {
		var b strings.Builder
		for _, problem := range problems {
			fmt.Fprintln(&b, problem.String())
		}
		return nil, errors.New("the material has problems:\n" + strings.TrimRight(b.String(), "\n"))
	}
	var lock *exp.Lock
	if p.Purpose == exp.PurposeMain {
		// X8.5: a main plan only starts when the material matches its lock. The file checks are offline and
		// run here; the environment checks follow in run(), where the API is available.
		lock, err = exp.LoadLock(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%s is missing: a main plan starts only with a lock (X8)", filepath.Join(dir, "plan.lock"))
			}
			return nil, err
		}
		if problems := lock.CheckFiles(dir); len(problems) > 0 {
			return nil, errors.New("the material deviates from plan.lock:\n" + problemLines(problems))
		}
	}
	k := p.K
	if opts.K > 0 {
		k = opts.K
	}
	seed := p.Seed
	if opts.SeedSet {
		seed = opts.Seed
	}
	cells, err := expCells(p, opts.Cells)
	if err != nil {
		return nil, err
	}
	digest, err := expMaterialDigest(dir)
	if err != nil {
		return nil, err
	}
	name := m.PlanID
	r := &expRunner{
		api: api, out: out, errw: errw,
		plan: p, tasks: tasks, manifests: manifests, specsDir: dir, planName: name, planSHA: digest, lock: lock,
		order:    expRunOrder(name, tasks, cells, k, seed),
		taskByID: map[string]exp.Task{}, cellByID: map[string]exp.Cell{}, models: map[string]string{},
		k: k, seed: seed, dryRun: opts.DryRun,
		poll: opts.Poll, limitGrace: opts.LimitGrace, startGrace: opts.StartGrace, heartbeat: opts.Heartbeat,
		now:           opts.Now,
		noSlotWait:    30 * time.Second,
		noSlotRetries: 20,
	}
	if r.poll <= 0 {
		r.poll = 10 * time.Second
	}
	if r.limitGrace <= 0 {
		r.limitGrace = 30 * time.Second
	}
	if r.startGrace <= 0 {
		r.startGrace = 2 * time.Minute
	}
	if r.heartbeat <= 0 {
		r.heartbeat = 60 * time.Second
	}
	if r.now == nil {
		r.now = time.Now
	}
	for _, t := range tasks {
		r.taskByID[t.ID] = t
	}
	for _, c := range p.Cells {
		r.cellByID[c.ID] = c
	}
	for _, m := range p.Models {
		r.models[m.Role] = m.ID
	}
	return r, nil
}

// expCells returns the cells to run, in the plan's order. --cell narrows them; an unknown id is an
// error instead of a silently smaller run.
func expCells(p *exp.Plan, want []string) ([]string, error) {
	known := make(map[string]bool, len(p.Cells))
	for _, c := range p.Cells {
		known[c.ID] = true
	}
	if len(want) == 0 {
		out := make([]string, 0, len(p.Cells))
		for _, c := range p.Cells {
			out = append(out, c.ID)
		}
		return out, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range want {
		if !known[id] {
			return nil, usagef("cell %q is not listed in the plan", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// expRunOrder builds the order of X1.6: per task and repetition the cells in a random order seeded
// from the plan's seed and the task/rep pair. Runs stay sequential (X1.7); only their assignment to
// time is shuffled, so that time of day and tariff do not coincide with one binding.
func expRunOrder(planName string, tasks []exp.Task, cells []string, k int, seed int64) []expPlannedRun {
	var out []expPlannedRun
	position := 0
	for _, t := range tasks {
		for rep := 1; rep <= k; rep++ {
			order := append([]string(nil), cells...)
			rng := newExpRand(seed, t.ID, rep)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			for _, cell := range order {
				position++
				out = append(out, expPlannedRun{
					Key:      fmt.Sprintf("%s/%s/%s/%d", planName, cell, t.ID, rep),
					Plan:     planName,
					Cell:     cell,
					Task:     t.ID,
					Rep:      rep,
					Seed:     seed,
					Position: position,
				})
			}
		}
	}
	return out
}

// expLatestByKey keeps the newest attempt of every run (X1.5): an attempt that a later one
// superseded is not the run's current state.
func expLatestByKey(runs []agwclient.ExpRun) map[string]agwclient.ExpRun {
	out := make(map[string]agwclient.ExpRun, len(runs))
	for _, run := range runs {
		if run.SupersededBy != "" {
			continue
		}
		key := run.Key
		if key == "" {
			key = run.ID
		}
		if old, ok := out[key]; !ok || run.ID != old.ID {
			out[key] = run
		}
	}
	return out
}

// expAttempt reads the attempt number of an id (<key> = 1, <key>#2 = 2).
func expAttempt(id, key string) int {
	if id == key {
		return 1
	}
	if rest, ok := strings.CutPrefix(id, key+"#"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n > 1 {
			return n
		}
	}
	return 1
}

func (r *expRunner) run(ctx context.Context) error {
	// #156: a mistyped model ID fails here, before the first chat is created.
	if err := r.checkModels(ctx); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s: %d runs (k=%d), seed %d, material %s\n", r.planName, len(r.order), r.k, r.seed, shortID(r.planSHA))
	if !r.dryRun {
		// #161: the protocol names what the plan has consumed and what it may consume in total.
		if usage, err := r.api.ExpUsage(ctx, r.planName); err != nil {
			fmt.Fprintf(r.errw, "plan usage not readable: %v\n", err)
		} else {
			fmt.Fprintf(r.out, "plan usage: %d tokens, %s (limit %d tokens, %s)\n", usage.TokensTotal,
				orDefault(usage.DurationTotal, "0s"), r.plan.Limits.PlanTokens(), roundDuration(r.plan.Limits.PlanDuration()))
		}
	}
	fmt.Fprintf(r.out, "run limits sent to the gateway: %s USD, %d tokens (input + output without cache reads), %s\n",
		strconv.FormatFloat(r.plan.Limits.MaxCostUSD, 'f', -1, 64), r.plan.Limits.MaxTokens, orDefault(r.plan.Limits.MaxDuration, "default"))
	if r.lock != nil && !r.dryRun {
		// X8.5: the material matches the lock; now the environment must as well. A dry run prints the order
		// without touching the gateway.
		env, err := r.api.ExpEnvironment(ctx)
		if err != nil {
			return fmt.Errorf("the experiment environment is not readable: %w", err)
		}
		if problems := exp.CompareEnvironment(r.lock, expEnvironment(env)); len(problems) > 0 {
			return errors.New("the environment deviates from plan.lock:\n" + problemLines(problems))
		}
		fmt.Fprintf(r.out, "environment: gateway %s, images match plan.lock\n", env.GatewayVersion)
	}
	counts := map[string]int{}
	return r.runAll(ctx, counts)
}

// checkModels compares the plan's model IDs with the gateway's catalogue. Without an API (dry run)
// there is nothing to compare against.
func (r *expRunner) checkModels(ctx context.Context) error {
	if r.api == nil {
		return nil
	}
	models, err := r.api.Models(ctx)
	if err != nil {
		return fmt.Errorf("model catalogue: %w", err)
	}
	known := make([]string, len(models))
	for i, model := range models {
		known[i] = model.ID
	}
	problems := exp.ValidateModels(r.plan, known)
	if len(problems) == 0 {
		return nil
	}
	var b strings.Builder
	for _, problem := range problems {
		fmt.Fprintln(&b, problem.String())
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// checkPlanLimits stops the plan when its total token or duration budget is used up (#161). It runs
// before every attempt that is not skipped, so the plan ends before a further chat is created.
func (r *expRunner) checkPlanLimits(ctx context.Context) error {
	usage, err := r.api.ExpUsage(ctx, r.planName)
	if err != nil {
		return fmt.Errorf("plan usage: %w", err)
	}
	if limit := r.plan.Limits.PlanTokens(); usage.TokensTotal >= limit {
		return fmt.Errorf("plan limit reached: tokens (%d of %d used)", usage.TokensTotal, limit)
	}
	var used time.Duration // an empty field means nothing has run yet
	if usage.DurationTotal != "" {
		var err error
		if used, err = time.ParseDuration(usage.DurationTotal); err != nil {
			return fmt.Errorf("plan usage: duration %q: %w", usage.DurationTotal, err)
		}
	}
	if limit := r.plan.Limits.PlanDuration(); used >= limit {
		return fmt.Errorf("plan limit reached: duration (%s of %s used)",
			roundDuration(used), roundDuration(limit))
	}
	return nil
}

// expEnvironment converts the client's answer into the material package's view.
func expEnvironment(e agwclient.ExpEnvironment) exp.Environment {
	out := exp.Environment{
		GatewayVersion: e.GatewayVersion, GatewayCommit: e.GatewayCommit, GatewayDescribe: e.GatewayDescribe,
		CatalogSHA256: e.CatalogSHA256, SkillsSHA256: e.SkillsSHA256, InstructionSHA256: e.InstructionSHA256,
		APIIndexSHA256: e.APIIndexSHA256, InferenceRuntime: e.InferenceRuntime, ImagesPi: e.ImagesPi, ImagesExec: e.ImagesExec,
	}
	for _, s := range e.Slots {
		out.Slots = append(out.Slots, exp.EnvironmentSlot{
			ID: s.ID, Variant: s.Variant, State: s.State, ChatID: s.ChatID, ImagePi: s.ImagePi, ImageExec: s.ImageExec,
		})
	}
	return out
}

// problemLines renders findings of the lock check, one per line.
func problemLines(problems []exp.Problem) string {
	lines := make([]string, 0, len(problems))
	for _, p := range problems {
		lines = append(lines, p.String())
	}
	return strings.Join(lines, "\n")
}

// newExpRand seeds the order of one task and repetition (X1.6): the plan's seed and a hash of
// task/rep, so that the same material always gives the same order.
func newExpRand(seed int64, task string, rep int) *rand.Rand {
	h := fnv.New64a()
	io.WriteString(h, task)
	io.WriteString(h, "/")
	io.WriteString(h, strconv.Itoa(rep))
	return rand.New(rand.NewPCG(uint64(seed), h.Sum64()))
}

func (r *expRunner) runAll(ctx context.Context, counts map[string]int) error {
	existing := map[string]agwclient.ExpRun{}
	if !r.dryRun {
		runs, err := r.api.ExpRuns(ctx, r.planName, "")
		if err != nil {
			return err
		}
		existing = expLatestByKey(runs)
	}
	// #214: say up front how many runs ended in an earlier start of this plan and are not run again.
	if ended := r.endedBefore(existing); ended > 0 {
		fmt.Fprintf(r.out, "plan %s: %d of %d runs ended before and are skipped\n", r.planName, ended, len(r.order))
	}
	executed, skipped := 0, 0
	for _, pr := range r.order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.dryRun {
			line := fmt.Sprintf("run %s (%s, task %s, rep %d)", pr.Key, pr.Cell, pr.Task, pr.Rep)
			if ids := r.fixtureIDs(pr); len(ids) > 0 {
				line += " fixtures: " + strings.Join(ids, ", ")
			}
			fmt.Fprintln(r.out, line)
			continue
		}
		prev, hasPrev := existing[pr.Key]
		out, err := r.runOne(ctx, pr, prev, hasPrev)
		if err != nil {
			return err
		}
		if out.skipped {
			skipped++
			continue
		}
		executed++
		counts[out.status]++
	}
	fmt.Fprintf(r.out, "%d runs: %d executed, %d skipped, %d done, %d error, %d aborted, %d limits\n",
		len(r.order), executed, skipped, counts["done"], counts["error"], counts["aborted"],
		counts["limit_cost"]+counts["limit_tokens"]+counts["limit_duration"])
	if skipped > 0 {
		fmt.Fprintf(r.out, "%d runs had ended before; a plan runs every run once. To run them again, copy the plan "+
			"under a new file name (the plan id is the file name).\n", skipped)
	}
	return nil
}

// endedBefore counts the runs of the plan whose latest attempt is over: runOne skips them (X1.5).
func (r *expRunner) endedBefore(existing map[string]agwclient.ExpRun) int {
	n := 0
	for _, pr := range r.order {
		if prev, ok := existing[pr.Key]; ok && prev.Status != expStarted {
			n++
		}
	}
	return n
}

type expOutcome struct {
	status  string
	skipped bool
}

// runOne executes one run: create the attempt and its chat, wait for the end, check, clean up and
// close the record. A run that the last process left in "started" is superseded and started again.
func (r *expRunner) runOne(ctx context.Context, pr expPlannedRun, prev agwclient.ExpRun, hasPrev bool) (expOutcome, error) {
	task, ok := r.taskByID[pr.Task]
	if !ok {
		return expOutcome{}, fmt.Errorf("task %q is not loaded", pr.Task)
	}
	cell, ok := r.cellByID[pr.Cell]
	if !ok {
		return expOutcome{}, fmt.Errorf("cell %q is not loaded", pr.Cell)
	}
	if hasPrev && prev.Status != expStarted {
		fmt.Fprintf(r.out, "run %s skipped (already ended in plan %s as %s)\n", pr.Key, r.planName, prev.Status)
		return expOutcome{status: prev.Status, skipped: true}, nil
	}
	// #161: the plan-wide limits are checked against what the plan has consumed so far, before the
	// next attempt and its chat exist. A skipped run above never reaches this point.
	if err := r.checkPlanLimits(ctx); err != nil {
		return expOutcome{}, err
	}
	id := pr.Key
	if hasPrev {
		id = fmt.Sprintf("%s#%d", pr.Key, expAttempt(prev.ID, pr.Key)+1)
	}
	// #207: the fixtures of the task are made fresh for every run and removed after it. This happens before the
	// run exists, so that a failing seed leaves no record and the plan can be started again.
	objects, release, err := r.seedRun(ctx, pr, id, task)
	if err != nil {
		return expOutcome{}, err
	}
	out, err := r.execute(ctx, pr, id, prev, hasPrev, task, cell, objects)
	if rerr := release(); rerr != nil {
		if err == nil {
			err = rerr
		} else {
			fmt.Fprintf(r.errw, "%v\n", rerr)
		}
	}
	return out, err
}

// execute runs one attempt from the supersede of an interrupted one to the closed record.
func (r *expRunner) execute(ctx context.Context, pr expPlannedRun, id string, prev agwclient.ExpRun, hasPrev bool,
	task exp.Task, cell exp.Cell, objects map[string]string) (expOutcome, error) {
	if hasPrev {
		if err := r.api.ExpSupersedeRun(ctx, prev.ID, id); err != nil {
			return expOutcome{}, fmt.Errorf("%s: supersede the interrupted attempt: %w", pr.Key, err)
		}
		fmt.Fprintf(r.out, "run %s: the interrupted attempt becomes aborted, starting %s\n", pr.Key, id)
	}
	model := r.models[cell.Model]
	run := agwclient.ExpRun{
		ID: id, Key: pr.Key, Plan: pr.Plan, PlanSHA256: r.planSHA, Cell: pr.Cell, Task: pr.Task,
		Rep: pr.Rep, Seed: pr.Seed, Position: pr.Position, Model: model, Status: expStarted,
	}
	if err := r.api.ExpStartRun(ctx, run); err != nil {
		return expOutcome{}, fmt.Errorf("%s: start: %w", pr.Key, err)
	}
	started := r.now()
	fail := func(status, reason string) (expOutcome, error) {
		if err := r.api.ExpEndRun(ctx, agwclient.ExpRunEnd{ID: id, Status: status, EndReason: reason}); err != nil {
			return expOutcome{}, fmt.Errorf("%s: ending the run failed: %w", pr.Key, err)
		}
		fmt.Fprintf(r.out, "run %s %s (%s)\n", pr.Key, status, reason)
		return expOutcome{status: status}, nil
	}

	chat, err := r.createChat(ctx, pr, task, cell, model, objects)
	if err != nil {
		if ctx.Err() != nil {
			return expOutcome{}, ctx.Err()
		}
		return fail("error", "chat: "+err.Error())
	}
	if r.lock != nil {
		// X8.5: the slot that carries this run must use the frozen images. The warm slots were checked before
		// the plan started; this catches a slot that started later (for example after a rebuild).
		if reason := r.checkSlot(ctx, chat.ID); reason != "" {
			r.abort(ctx, chat.ID)
			return fail("error", reason)
		}
	}
	if err := r.api.ExpSetRunChat(ctx, id, chat.ID); err != nil {
		return expOutcome{}, fmt.Errorf("%s: recording the chat: %w", pr.Key, err)
	}
	status, reason, det, err := r.waitEnd(ctx, chat.ID)
	if err != nil {
		if ctx.Err() != nil {
			// The run stays "started"; the next start supersedes it (X1.5).
			return expOutcome{}, ctx.Err()
		}
		return fail("error", "wait: "+err.Error())
	}
	created, objsErr := r.createdObjects(ctx, chat.ID)
	if objsErr != nil {
		fmt.Fprintf(r.errw, "run %s: provenance register not readable: %v\n", pr.Key, objsErr)
	}
	pass, evidence, checkErr := exp.Evaluate(ctx, expRunnerCheck{api: r.api, chat: chat.ID}, task.Check, created)
	if checkErr != nil {
		status, reason = "error", "check: "+checkErr.Error()
	}
	r.cleanup(ctx, chat.ID, det)
	if err := r.api.ExpEndRun(ctx, agwclient.ExpRunEnd{ID: id, Status: status, EndReason: reason,
		Pass: &pass, CheckEvidence: evidence}); err != nil {
		return expOutcome{}, fmt.Errorf("%s: ending the run failed: %w", pr.Key, err)
	}
	fmt.Fprintf(r.out, "run %s %s pass=%s chat=%s %s tokens %d (+%d cache reads)\n", pr.Key, status, yesNo(pass), shortID(chat.ID),
		roundDuration(r.now().Sub(started)), det.Chat.Tokens.Input+det.Chat.Tokens.Output, det.Chat.Tokens.CacheRead)
	if reason != "" && status != "done" {
		fmt.Fprintf(r.errw, "  %s\n", reason)
	}
	return expOutcome{status: status}, nil
}

func roundDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// experimentVariant maps the plan's binding to the variant key the toolset parser accepts. The
// plan format writes the combination as "cli+mcp"; the canonical key is "cli,mcp".
func experimentVariant(binding string) string {
	if binding == "cli+mcp" {
		return "cli,mcp"
	}
	return binding
}

// createChat makes the run's experiment chat. The task text goes with the create request: the
// experiment key may create and read chats, not post messages (X1.3).
func (r *expRunner) createChat(ctx context.Context, pr expPlannedRun, task exp.Task, cell exp.Cell, model string, objects map[string]string) (agwclient.Chat, error) {
	delegation, err := exp.ResolveFixtureRefs(task.Delegation, objects)
	if err != nil {
		return agwclient.Chat{}, fmt.Errorf("delegation: %w", err)
	}
	req := agwclient.CreateChatRequest{
		Model:      model,
		Variant:    experimentVariant(cell.Binding),
		Title:      truncate(fmt.Sprintf("%s · %s #%d", task.Title, cell.ID, pr.Rep), 80),
		Message:    task.Prompt,
		Delegation: delegation,
		Experiment: &agwclient.ExperimentProfile{
			Protection: cell.Protection,
			Cell:       cell.ID,
			Run:        pr.Key,
			Seed:       pr.Seed,
			// X7.6/#152: on unless the cell turns it off; the plan's field is per cell.
			Trace: exp.TraceValue(cell.Trace),
			// #206: the gateway enforces the plan's run limits, not its own defaults.
			Limits: &agwclient.ExpRunLimits{MaxCostUSD: r.plan.Limits.MaxCostUSD, MaxTokens: r.plan.Limits.MaxTokens,
				MaxDuration: r.plan.Limits.MaxDuration},
		},
	}
	for attempt := 0; ; attempt++ {
		chat, err := r.api.CreateChat(ctx, req)
		if err == nil {
			return chat, nil
		}
		if !errors.Is(err, agwclient.ErrNoSlot) || attempt >= r.noSlotRetries {
			return agwclient.Chat{}, err
		}
		fmt.Fprintf(r.errw, "run %s: no free slot, waiting %s\n", pr.Key, r.noSlotWait)
		select {
		case <-ctx.Done():
			return agwclient.Chat{}, ctx.Err()
		case <-time.After(r.noSlotWait):
		}
	}
}

// waitEnd follows a run until it is over (X1.8): the chat is quiet, no observed job is running, and
// no limit of the plan applies. The gateway is the only place that enforces a run limit (#213); the runner
// reads its reason and only reports a limit the gateway did not enforce. It returns the end reason of the chat's last turn, the reason text
// and the last view of the chat.
func (r *expRunner) waitEnd(ctx context.Context, chatID string) (status, reason string, det agwclient.ChatDetail, err error) {
	maxDur, err := time.ParseDuration(r.plan.Limits.MaxDuration)
	if err != nil {
		return "", "", agwclient.ChatDetail{}, fmt.Errorf("limits.max_duration: %w", err)
	}
	startedAt := r.now()
	deadline := startedAt.Add(maxDur)
	seenActive := false
	var overSince time.Time
	lastBeat := startedAt
	for {
		det, err = r.api.Chat(ctx, chatID)
		if err != nil {
			return "", "", det, err
		}
		jobs, err := r.api.Jobs(ctx, chatID)
		if err != nil {
			return "", "", det, err
		}
		open := 0
		for _, job := range jobs {
			if job.State == "running" {
				open++
			}
		}
		c := det.Chat
		if c.Running || c.Resuming || c.Queued > 0 || c.LLMCalls > 0 || expHasAssistant(det) {
			seenActive = true
		}
		now := r.now()
		// X3, #213: the gateway enforces the run's limits and stores the reason at the chat; that reason is
		// the run's end. The runner never sets a limit_* status itself.
		if c.Experiment != nil && c.Experiment.Limit != "" {
			return expLimitStatus(c.Experiment.Limit),
				expLimitReason(c.Experiment.Limit, r.plan.Limits, c, now.Sub(startedAt)), det, nil
		}
		// Watch only: a limit that stays exceeded although the gateway should have ended the run is a defect
		// of the gateway, reported as an error and not as a limit.
		if kind, value, bound := expOverLimit(r.plan.Limits, c, now, startedAt, deadline); kind != "" {
			if overSince.IsZero() {
				overSince = now
			}
			if now.Sub(overSince) >= r.limitGrace {
				r.abort(ctx, chatID)
				return "error", fmt.Sprintf("run limit not enforced by the gateway: %s %s of %s after %s",
					kind, value, bound, roundDuration(r.limitGrace)), det, nil
			}
		} else {
			overSince = time.Time{}
		}
		if !c.Running && !c.Resuming && c.Queued == 0 && open == 0 {
			if seenActive {
				status, reason = expEndReason(det)
				return status, reason, det, nil
			}
			if now.Sub(startedAt) >= r.startGrace {
				return "error", "the chat did not start", det, nil
			}
		}
		if now.Sub(lastBeat) >= r.heartbeat {
			fmt.Fprintf(r.errw, "run still going: %s, tokens %d (+%d cache reads), %s\n", roundDuration(now.Sub(startedAt)),
				c.Tokens.Input+c.Tokens.Output, c.Tokens.CacheRead, fmtCost(c.Cost))
			lastBeat = now
		}
		select {
		case <-ctx.Done():
			return "", "", det, ctx.Err()
		case <-time.After(r.poll):
		}
	}
}

// expLimitStatus maps the reason the gateway stored (X3.5) to the run's status.
// expOverLimit names the first limit of the plan that the chat has passed (kind, measured value, limit), else "".
func expOverLimit(lim exp.Limits, c agwclient.Chat, now, startedAt, deadline time.Time) (kind, value, bound string) {
	switch {
	case lim.MaxCostUSD > 0 && c.Cost >= lim.MaxCostUSD:
		return "cost", fmt.Sprintf("%.4f USD", c.Cost), fmt.Sprintf("%.4f USD", lim.MaxCostUSD)
	case lim.MaxTokens > 0 && c.Tokens.Input+c.Tokens.Output >= lim.MaxTokens:
		return "tokens", fmt.Sprintf("%d", c.Tokens.Input+c.Tokens.Output), fmt.Sprintf("%d", lim.MaxTokens)
	case !deadline.Equal(startedAt) && !now.Before(deadline):
		return "duration", roundDuration(now.Sub(startedAt)), lim.MaxDuration
	}
	return "", "", ""
}

// expLimitReason is the uniform end reason of a run the gateway ended at a limit (#213).
func expLimitReason(limit string, lim exp.Limits, c agwclient.Chat, elapsed time.Duration) string {
	bound := ""
	switch limit {
	case "cost":
		bound = fmt.Sprintf("%.4f USD", lim.MaxCostUSD)
	case "tokens":
		bound = fmt.Sprintf("%d tokens", lim.MaxTokens)
	case "duration":
		bound = lim.MaxDuration
	}
	return fmt.Sprintf("gateway limit %s (%s): tokens %d (+%d cache reads), %.4f USD, %s", limit, bound,
		c.Tokens.Input+c.Tokens.Output, c.Tokens.CacheRead, c.Cost, roundDuration(elapsed))
}

func expLimitStatus(limit string) string {
	switch limit {
	case "cost":
		return "limit_cost"
	case "tokens":
		return "limit_tokens"
	case "duration":
		return "limit_duration"
	}
	return "limit_" + limit
}

// checkSlot verifies the image IDs of the slot that carries the chat against the lock (X8.5). It returns
// an empty string when the slot matches and an end reason otherwise.
func (r *expRunner) checkSlot(ctx context.Context, chatID string) string {
	env, err := r.api.ExpEnvironment(ctx)
	if err != nil {
		return "environment: " + err.Error()
	}
	slot, ok := expEnvironment(env).SlotForChat(chatID)
	if !ok {
		return "environment: the slot of chat " + shortID(chatID) + " is not visible; its images cannot be verified"
	}
	problems := exp.CompareSlot(r.lock, slot)
	if len(problems) == 0 {
		return ""
	}
	return "environment: " + problemLines(problems)
}

func (r *expRunner) abort(ctx context.Context, chatID string) {
	if err := r.api.ExpAbortChat(ctx, chatID); err != nil {
		fmt.Fprintf(r.errw, "abort of chat %s failed: %v\n", shortID(chatID), err)
	}
}

// expEndReason reads the outcome of the chat's last assistant turn. An error that pi reports there
// is the run's error; an abort is the run's abort.
func expEndReason(det agwclient.ChatDetail) (status, reason string) {
	for i := len(det.Messages) - 1; i >= 0; i-- {
		m := det.Messages[i]
		if m.Role != "assistant" {
			continue
		}
		var p struct {
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		}
		_ = json.Unmarshal(m.Message, &p)
		switch p.StopReason {
		case "error":
			return "error", p.ErrorMessage
		case "aborted":
			return "aborted", "the turn was aborted"
		}
		return "done", ""
	}
	return "done", ""
}

func expHasAssistant(det agwclient.ChatDetail) bool {
	for _, m := range det.Messages {
		if m.Role == "assistant" {
			return true
		}
	}
	return false
}

// expRunnerCheck asks the gateway for a read call with the experiment account's token (X1.9).
type expRunnerCheck struct {
	api  expAPI
	chat string
}

func (c expRunnerCheck) Get(ctx context.Context, path string) (json.RawMessage, error) {
	res, err := c.api.ExpCall(ctx, agwclient.ExpCallRequest{Chat: c.chat, Method: "GET", Path: path})
	if err != nil {
		return nil, err
	}
	if res.Status != "ok" || res.HTTPStatus >= 400 {
		msg := res.Message
		if msg == "" {
			msg = fmt.Sprintf("platform responds %d", res.HTTPStatus)
		}
		return nil, errors.New(msg)
	}
	return json.RawMessage(res.Body), nil
}

// LastAssistantText reads the stored chat and returns the text of its last assistant message
// (#147). Tool outputs, thinking, tool calls and system notes are not part of it.
func (c expRunnerCheck) LastAssistantText(ctx context.Context) (string, error) {
	detail, err := c.api.Chat(ctx, c.chat)
	if err != nil {
		return "", err
	}
	messages := make([]exp.Message, len(detail.Messages))
	for i, m := range detail.Messages {
		messages[i] = exp.Message{Role: m.Role, Message: m.Message}
	}
	return exp.AssistantText(messages), nil
}

// createdObjects reads the chat's provenance register (X1.10). The check and the cleanup use only
// these identifiers, never a name.
func (r *expRunner) createdObjects(ctx context.Context, chatID string) ([]exp.Object, error) {
	objs, err := r.api.ExpObjects(ctx, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]exp.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, exp.Object{Resource: o.Resource, ID: o.ID})
	}
	return out, nil
}

// cleanup removes what the run created, in the order of CLAUDE.md (image, training container,
// model, dataset) and only by identifiers from the provenance register or from the training
// container ids in the chat's answers. A failure is reported and does not change the run's result;
// the fixtures (X4) are removed by the runner after each run (#207), not here.
func (r *expRunner) cleanup(ctx context.Context, chatID string, det agwclient.ChatDetail) {
	objs, err := r.api.ExpObjects(ctx, chatID)
	if err != nil {
		fmt.Fprintf(r.errw, "cleanup of chat %s: register not readable: %v\n", shortID(chatID), err)
		return
	}
	byResource := map[string][]string{}
	for _, o := range objs {
		byResource[o.Resource] = appendUnique(byResource[o.Resource], o.ID)
	}
	for _, id := range byResource["container_image"] {
		r.deleteObject(ctx, chatID, "/container-images/"+url.PathEscape(id), "")
	}
	for _, id := range expContainerIDs(det) {
		r.deleteObject(ctx, chatID, "/train/containers/"+url.PathEscape(id), "")
	}
	for _, id := range byResource["model"] {
		r.deleteObject(ctx, chatID, "/models/"+url.PathEscape(id), "")
	}
	for _, id := range byResource["dataset"] {
		// DELETE /datasets/{id} wants a CVAT login in the body; placeholders suffice without an
		// annotation task (CLAUDE.md, cleanup of test objects).
		r.deleteObject(ctx, chatID, "/datasets/"+url.PathEscape(id), `{"key":"-","csrftoken":"-","sessionid":"-"}`)
	}
}

func (r *expRunner) deleteObject(ctx context.Context, chatID, path, body string) {
	req := agwclient.ExpCallRequest{Chat: chatID, Method: "DELETE", Path: path}
	if body != "" {
		req.Body = json.RawMessage(body)
	}
	res, err := r.api.ExpCall(ctx, req)
	if err != nil {
		fmt.Fprintf(r.errw, "cleanup: DELETE %s failed: %v\n", path, err)
		return
	}
	if res.Status != "ok" || res.HTTPStatus >= 300 {
		fmt.Fprintf(r.errw, "cleanup: DELETE %s: %s\n", path, orDefault(res.Message, fmt.Sprintf("HTTP %d", res.HTTPStatus)))
	}
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// expContainerIDs collects the training container ids from the chat's answers. The provenance
// register records a training start as a task, not as the container (delegation.Created), so the
// container ids come from the answers to POST /train/config; nothing is inferred from a name.
func expContainerIDs(det agwclient.ChatDetail) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range det.Messages {
		dec := json.NewDecoder(bytes.NewReader(m.Message))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			continue
		}
		walkJSON(v, func(key string, val any) {
			if key != "container_id" {
				return
			}
			id := ""
			switch x := val.(type) {
			case json.Number:
				id = x.String()
			case string:
				id = x
			case float64:
				id = strconv.FormatFloat(x, 'f', -1, 64)
			}
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		})
	}
	return out
}

// walkJSON visits every key/value pair in a decoded JSON document.
func walkJSON(v any, fn func(key string, val any)) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			fn(k, val)
			walkJSON(val, fn)
		}
	case []any:
		for _, e := range x {
			walkJSON(e, fn)
		}
	}
}

// expMaterialDigest names the material the plan uses: the same file set as plan.lock (X8.3), without
// result folders and local caches, in a stable order. It identifies the state a run was started with.
func expMaterialDigest(dir string) (string, error) {
	files, err := exp.MaterialFiles(dir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		io.WriteString(h, name)
		h.Write([]byte{0})
		io.WriteString(h, files[name])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fixtureIDs lists the manifest ids a run seeds.
func (r *expRunner) fixtureIDs(pr expPlannedRun) []string {
	task, ok := r.taskByID[pr.Task]
	if !ok {
		return nil
	}
	var ids []string
	for _, m := range exp.ForTask(r.manifests, task, pr.Cell) {
		ids = append(ids, m.ID)
	}
	return ids
}

// seedRun makes the fixtures of a run: remove leftovers of the cell, create the task's fixtures for run id
// and read them back. It returns the object identifier per manifest and the function that removes the
// fixtures after the run. A failure is a failure of the environment, not a result of the run (#207).
func (r *expRunner) seedRun(ctx context.Context, pr expPlannedRun, id string, task exp.Task) (map[string]string, func() error, error) {
	noop := func() error { return nil }
	manifests := exp.ForTask(r.manifests, task, pr.Cell)
	if len(manifests) == 0 {
		return nil, noop, nil
	}
	started := r.now()
	left, err := r.api.ExpUnseed(ctx, pr.Plan, pr.Cell)
	if err != nil {
		return nil, noop, fmt.Errorf("run %s: fixtures of cell %s could not be removed: %w; the plan stops", id, pr.Cell, err)
	}
	if left.Removed > 0 {
		fmt.Fprintf(r.out, "run %s: removed %d leftover fixtures of cell %s\n", id, left.Removed, pr.Cell)
	}
	specs, err := exp.Specs(r.specsDir, manifests)
	if err != nil {
		return nil, noop, fmt.Errorf("run %s: fixtures of cell %s could not be read: %w", id, pr.Cell, err)
	}
	ans, status, err := r.api.ExpSeed(ctx, pr.Plan, pr.Cell, id, specs)
	if err != nil || status == 409 {
		// Whatever was created stays recorded by identifier; remove it before stopping.
		if _, uerr := r.api.ExpUnseed(context.WithoutCancel(ctx), pr.Plan, pr.Cell); uerr != nil {
			fmt.Fprintf(r.errw, "run %s: removing the partly seeded fixtures failed: %v\n", id, uerr)
		}
		if status == 409 {
			var b strings.Builder
			for _, m := range ans.Mismatches {
				if m.Missing {
					fmt.Fprintf(&b, "\n  %s (%s %s): object is missing", m.ManifestID, m.Kind, m.ObjectID)
				}
				for _, f := range m.Fields {
					fmt.Fprintf(&b, "\n  %s (%s %s): field %s expected sha256 %s, read back %s",
						m.ManifestID, m.Kind, m.ObjectID, f.Field, f.Expected, f.Actual)
				}
			}
			return nil, noop, fmt.Errorf("run %s: fixtures of cell %s could not be seeded: fixture_mismatch:%s; the plan stops before this run",
				id, pr.Cell, b.String())
		}
		return nil, noop, fmt.Errorf("run %s: fixtures of cell %s could not be seeded: %w; the plan stops before this run", id, pr.Cell, err)
	}
	objects := map[string]string{}
	var names []string
	for _, f := range ans.Fixtures {
		objects[f.ManifestID] = f.ObjectID
		names = append(names, f.ManifestID)
	}
	fmt.Fprintf(r.out, "run %s: fixtures %s seeded (%s)\n", id, strings.Join(names, ", "), roundDuration(r.now().Sub(started)))
	release := func() error {
		// Also after an interrupt: the fixtures must not outlive the run.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if _, err := r.api.ExpUnseed(rctx, pr.Plan, pr.Cell); err != nil {
			return fmt.Errorf("run %s: fixtures of cell %s could not be removed: %w; the plan stops", id, pr.Cell, err)
		}
		return nil
	}
	return objects, release, nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed watch_schema.sql
var watchSchemaSQL string

func init() { registerSchema(watchSchemaSQL) }

// States of an observed job.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// WatchJob is a row of watch_jobs: a platform job the gateway observes (autonomy A-E2). Inside a plan
// the plan step names it (plan_steps.watch_job) and waits in state "waiting" until it ends; outside a
// plan it is only shown (no note, no wake-up).
type WatchJob struct {
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	PlanID     string `json:"plan_id,omitempty"`
	StepIdx    int    `json:"step_idx,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	State      string `json:"state"`
	// Progress is the percent the platform reported; nil while it reports none (D2.4).
	Progress *float64      `json:"progress,omitempty"`
	Message  string        `json:"message,omitempty"`
	ETA      *time.Time    `json:"eta,omitempty"`
	LogTail  string        `json:"log_tail,omitempty"`
	Outputs  []WatchOutput `json:"outputs,omitempty"`
	// Link is the address the user can open for the job (own code C-E4); empty for the kinds that know
	// none, and the card never builds one of its own.
	Link string `json:"link,omitempty"`
	// FailReads counts reading calls that failed in a row (D2.3).
	FailReads int        `json:"fail_reads,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// WatchOutput is an object a job produced ({Resource, ID, Label} of watch.Output).
type WatchOutput struct {
	Resource string `json:"resource"`
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
}

// WatchProgress is what one poll found; every field is written to the job's row.
type WatchProgress struct {
	State    string
	Percent  *float64
	Message  string
	LogTail  string
	Outputs  []WatchOutput
	Link     string
	ETA      *time.Time
	ReadFail bool // the reading call failed: the row keeps its state and counts the failure
}

const watchJobCols = `id, chat_id::text, plan_id, step_idx, tool_call_id, kind, ref, state, progress, message, eta, log_tail, outputs, link, fail_reads, started_at, checked_at, ended_at`

func scanWatchJob(row interface{ Scan(...any) error }) (WatchJob, error) {
	var j WatchJob
	var outputs string
	err := row.Scan(&j.ID, &j.ChatID, &j.PlanID, &j.StepIdx, &j.ToolCallID, &j.Kind, &j.Ref, &j.State,
		&j.Progress, &j.Message, &j.ETA, &j.LogTail, &outputs, &j.Link, &j.FailReads, &j.StartedAt, &j.CheckedAt, &j.EndedAt)
	if err != nil {
		return j, err
	}
	if outputs != "" && outputs != "[]" {
		if err := json.Unmarshal([]byte(outputs), &j.Outputs); err != nil {
			return j, err
		}
	}
	return j, nil
}

// newJobID draws the identifier of a job ("w" + 12 hex).
func newJobID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "w" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "w" + hex.EncodeToString(b[:])
}

// AddWatchJob stores a new observed job in state "running" and returns its id.
func (s *Store) AddWatchJob(ctx context.Context, j WatchJob) (string, error) {
	id := j.ID
	if id == "" {
		id = newJobID()
	}
	outputs := "[]"
	if len(j.Outputs) > 0 {
		b, err := json.Marshal(j.Outputs)
		if err != nil {
			return "", err
		}
		outputs = string(b)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO watch_jobs (id, chat_id, plan_id, step_idx, tool_call_id, kind, ref, outputs)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, j.ChatID, j.PlanID, j.StepIdx, j.ToolCallID, j.Kind, j.Ref, outputs)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetWatchJob returns one job of a chat; ErrNotFound for an unknown id or another chat's job.
func (s *Store) GetWatchJob(ctx context.Context, chatID, jobID string) (WatchJob, error) {
	j, err := scanWatchJob(s.pool.QueryRow(ctx, `SELECT `+watchJobCols+` FROM watch_jobs WHERE id=$1 AND chat_id=$2`, jobID, chatID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WatchJob{}, ErrNotFound
		}
		return WatchJob{}, err
	}
	return j, nil
}

// OpenWatchJobs returns every running job of every chat, oldest first. The loop stops observing a job
// whose step does not wait any more (the plan ended in between): it closes the row and touches no step.
func (s *Store) OpenWatchJobs(ctx context.Context) ([]WatchJob, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+watchJobCols+` FROM watch_jobs w WHERE w.state='running' ORDER BY w.started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WatchJob{}
	for rows.Next() {
		j, err := scanWatchJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ChatWatchJobs returns the jobs of one chat, newest first (the "jobs" route and the cards).
func (s *Store) ChatWatchJobs(ctx context.Context, chatID string) ([]WatchJob, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+watchJobCols+` FROM watch_jobs WHERE chat_id=$1 ORDER BY started_at DESC, id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WatchJob{}
	for rows.Next() {
		j, err := scanWatchJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetJobProgress writes what a poll found and marks the row as checked. A failed reading call counts
// up fail_reads and touches nothing else; a successful one clears the counter.
func (s *Store) SetJobProgress(ctx context.Context, jobID string, p WatchProgress) error {
	if p.ReadFail {
		_, err := s.pool.Exec(ctx, `UPDATE watch_jobs SET fail_reads=fail_reads+1, checked_at=now() WHERE id=$1 AND state='running'`, jobID)
		return err
	}
	outputs := "[]"
	if len(p.Outputs) > 0 {
		b, err := json.Marshal(p.Outputs)
		if err != nil {
			return err
		}
		outputs = string(b)
	}
	state := p.State
	if state == "" {
		state = JobRunning
	}
	var ended any
	if state != JobRunning {
		ended = time.Now()
	}
	_, err := s.pool.Exec(ctx, `UPDATE watch_jobs SET state=$2, progress=$3, message=$4, eta=$5, log_tail=$6, outputs=$7,
fail_reads=0, checked_at=now(), ended_at=$8, link=$9 WHERE id=$1 AND state='running'`,
		jobID, state, p.Percent, p.Message, p.ETA, p.LogTail, outputs, ended, p.Link)
	return err
}

// SetJobMessage writes a message of the watcher itself (a failure it found, D2.3), without touching
// the state or the progress.
func (s *Store) SetJobMessage(ctx context.Context, jobID, message string) error {
	_, err := s.pool.Exec(ctx, `UPDATE watch_jobs SET message=$2, checked_at=now() WHERE id=$1 AND state='running'`, jobID, message)
	return err
}

// ReArmWatchJob replaces the kind and the reference of a running job (the chain of a started training:
// the task ends when the container is up, the run is then observed itself) and clears what the first
// kind reported.
func (s *Store) ReArmWatchJob(ctx context.Context, jobID, kind, ref string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE watch_jobs SET kind=$2, ref=$3, progress=NULL, message='', eta=NULL, log_tail='',
outputs='[]', link='', fail_reads=0, checked_at=NULL WHERE id=$1 AND state='running'`, jobID, kind, ref)
	return tag.RowsAffected() > 0, err
}

// FinishWatchJob ends a job with "done" or "failed"; ok is false if it was not running any more.
func (s *Store) FinishWatchJob(ctx context.Context, jobID, state string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE watch_jobs SET state=$2, ended_at=now(), fail_reads=0 WHERE id=$1 AND state='running'`, jobID, state)
	return tag.RowsAffected() > 0, err
}

// WaitPlanStep sets a claimed step to "waiting", names the job it waits for and records what the call
// that started the job answered (D2.2); the object the call created is needed by later steps. ok is
// false if the step was neither running nor open (finished elsewhere).
func (s *Store) WaitPlanStep(ctx context.Context, planID string, idx int, jobID, result, resource, id string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state='waiting', watch_job=$3, result=$4, created_resource=$5, created_id=$6
WHERE plan_id=$1 AND idx=$2 AND state IN ('running','open')`, planID, idx, jobID, result, resource, id)
	return tag.RowsAffected() > 0, err
}

// ParkApproval marks a step as waiting for a parked approval (D2.8): the user decides later, the
// gateway repeats the stored call on approval. ok is false if the step is not open or running.
func (s *Store) ParkApproval(ctx context.Context, planID string, idx int, approvalID string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET parked_approval=$3 WHERE plan_id=$1 AND idx=$2 AND state IN ('open','running')`,
		planID, idx, approvalID)
	return tag.RowsAffected() > 0, err
}

// PlanStepByParkedApproval returns the plan step a parked approval belongs to; ErrNotFound if no step
// of this chat carries that approval.
func (s *Store) PlanStepByParkedApproval(ctx context.Context, chatID, approvalID string) (PlanStep, error) {
	var planID string
	var idx int
	err := s.pool.QueryRow(ctx, `SELECT s.plan_id::text, s.idx FROM plan_steps s JOIN plans p ON p.id = s.plan_id
WHERE p.chat_id=$1 AND s.parked_approval=$2 AND s.parked_approval<>'' ORDER BY p.created_at DESC LIMIT 1`, chatID, approvalID).Scan(&planID, &idx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanStep{}, ErrNotFound
		}
		return PlanStep{}, err
	}
	p, err := s.GetPlan(ctx, planID)
	if err != nil {
		return PlanStep{}, err
	}
	for _, st := range p.Steps {
		if st.Idx == idx {
			return st, nil
		}
	}
	return PlanStep{}, ErrNotFound
}

// CloseWatchJobsOfPlan ends the running jobs of a plan whose steps will not continue (cancelled,
// superseded, expired): the platform job keeps running, the observation ends with "failed" so that no
// step waits forever. ok counts the closed rows.
func (s *Store) CloseWatchJobsOfPlan(ctx context.Context, planID string) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE watch_jobs SET state='failed', message='plan ended', ended_at=now()
WHERE plan_id=$1 AND state='running'`, planID)
	return int(tag.RowsAffected()), err
}

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
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed search_schema.sql
var searchSchemaSQL string

func init() { registerSchema(searchSchemaSQL) }

// ErrSearchRunning: the chat already has a running search (the unique index searches_one_running).
var ErrSearchRunning = errors.New("a training search is already running in this chat")

// Limits of what the book of trials keeps per trial (issue #168). Above them the older half is thinned out 2:1,
// so that a long run keeps its whole curve in coarser steps.
const (
	MaxTrialEpochs = 1000
	MaxTrialGPU    = 2000
)

// States of a search and of a trial.
const (
	SearchProposed = "proposed"
	SearchRunning  = "running"
	SearchEnded    = "ended"
	SearchRejected = "rejected"
	SearchExpired  = "expired"

	TrialDeclared = "declared"
	TrialRunning  = "running"
	TrialDone     = "done"
	TrialFailed   = "failed"
	TrialStopped  = "stopped"
	TrialRefused  = "refused"
)

// Paths of a search and of its trials: a platform training template or the chat's notebook server.
const (
	SearchPathTemplate = "template"
	SearchPathNotebook = "notebook"
)

// SearchFile is a file of the code of a notebook search, bound by its SHA-256 (the gateway computes it from the
// bytes it read, never from a field).
type SearchFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Text is the text of the approved file (UTF-8 and not too large), kept so that a later change can be shown as a
	// diff in the question to the user; empty for a binary or large file.
	Text string `json:"text,omitempty"`
}

// SearchCode is the code a notebook search was approved with: the files with their hashes, the entry, the
// arguments, the datasets and the time limit. Version counts up with every approved change of the code.
type SearchCode struct {
	Version        int          `json:"version,omitempty"`
	Entry          string       `json:"entry,omitempty"`
	Files          []SearchFile `json:"files,omitempty"`
	DatasetIDs     []int64      `json:"dataset_ids,omitempty"`
	Args           []string     `json:"args,omitempty"`
	TimeoutMinutes int          `json:"timeout_minutes,omitempty"`
}

// Search is a training search with a budget (issue #168): the user approves a search space and a budget once,
// and the agent runs trial after trial inside them. Objective, Space, Budget, Base and Holdout are stored as the
// JSON the internal/search package and the chat package read and write; the store does not interpret them.
type Search struct {
	ID        string          `json:"id"`
	ChatID    string          `json:"chat_id"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	Path      string          `json:"path"`
	Objective json.RawMessage `json:"objective"`
	Space     json.RawMessage `json:"space"`
	Budget    json.RawMessage `json:"budget"`
	Base      json.RawMessage `json:"base,omitempty"`
	Code      SearchCode      `json:"code"`
	Holdout   json.RawMessage `json:"holdout,omitempty"`
	// Note is the agent's own text, shown as not verified.
	Note       string `json:"note,omitempty"`
	EndReason  string `json:"end_reason,omitempty"`
	WinnerIdx  *int   `json:"winner_idx,omitempty"`
	Session    string `json:"session,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// AutoTurns counts the turns the agent was woken for by this search (against its wake limit).
	AutoTurns int `json:"auto_turns"`

	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndsBy    *time.Time `json:"ends_by,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Trial is one run of the book of trials: a trial of a search (SearchID set) or a single run of the chat.
// Seq numbers the trials of a chat from 1, Idx those of a search from 1 (0: single run). Ref is the training
// container (template) or the run (notebook); JobID the observed job.
type Trial struct {
	ID          string          `json:"id"`
	ChatID      string          `json:"chat_id"`
	SearchID    string          `json:"search_id,omitempty"`
	Seq         int             `json:"seq"`
	Idx         int             `json:"idx"`
	Path        string          `json:"path"`
	State       string          `json:"state"`
	Params      json.RawMessage `json:"params"`
	Note        string          `json:"note,omitempty"`
	Ref         string          `json:"ref,omitempty"`
	JobID       string          `json:"job_id,omitempty"`
	CodeVersion int             `json:"code_version,omitempty"`
	Source      string          `json:"source,omitempty"`
	Epoch       *int            `json:"epoch,omitempty"`
	Epochs      *int            `json:"epochs,omitempty"`
	Score       *float64        `json:"score,omitempty"`
	ScoreEpoch  *int            `json:"score_epoch,omitempty"`
	GPU         json.RawMessage `json:"gpu,omitempty"`
	// Figures: "" unknown, "none" the training template writes no figures, "ok" it does.
	Figures string `json:"figures,omitempty"`
	// GPUAvailable: the run sampled a GPU; nil while unknown.
	GPUAvailable *bool      `json:"gpu_available,omitempty"`
	Error        string     `json:"error,omitempty"`
	StoppedBy    string     `json:"stopped_by,omitempty"`
	DeclaredAt   time.Time  `json:"declared_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
}

// TrialEpoch is the line of one epoch; Data holds the figures (train_loss, val_acc, ...) as the run wrote them.
type TrialEpoch struct {
	TrialID string          `json:"trial_id,omitempty"`
	Epoch   int             `json:"epoch"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data"`
}

// TrialGPU is one sample of the GPU during a trial.
type TrialGPU struct {
	TrialID    string    `json:"trial_id,omitempty"`
	At         time.Time `json:"at"`
	Util       float64   `json:"util"`
	MemUsedMB  int       `json:"mem_used_mb"`
	MemTotalMB int       `json:"mem_total_mb"`
}

// SearchEvent is one line of the record of a search (approved, trial_declared, trial_refused, code_approved,
// budget_changed, hint, stopped, adopted, ended, final).
type SearchEvent struct {
	ID       int64           `json:"id"`
	SearchID string          `json:"search_id"`
	At       time.Time       `json:"at"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data,omitempty"`
}

func newHexID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return prefix + hex.EncodeToString(b[:])
}

func jsonOr(raw json.RawMessage, empty string) string {
	if len(raw) == 0 {
		return empty
	}
	return string(raw)
}

const searchCols = `id, chat_id::text, title, state, path, objective::text, space::text, budget::text, base::text, code::text, holdout::text,
note, end_reason, winner_idx, session, approval_id, tool_call_id, auto_turns, created_at, started_at, ends_by, ended_at`

func scanSearch(row pgx.Row) (Search, error) {
	var s Search
	var objective, space, budget, base, code, holdout string
	err := row.Scan(&s.ID, &s.ChatID, &s.Title, &s.State, &s.Path, &objective, &space, &budget, &base, &code, &holdout,
		&s.Note, &s.EndReason, &s.WinnerIdx, &s.Session, &s.ApprovalID, &s.ToolCallID, &s.AutoTurns, &s.CreatedAt, &s.StartedAt, &s.EndsBy, &s.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	s.Objective, s.Space, s.Budget = json.RawMessage(objective), json.RawMessage(space), json.RawMessage(budget)
	if base != "{}" {
		s.Base = json.RawMessage(base)
	}
	if holdout != "{}" {
		s.Holdout = json.RawMessage(holdout)
	}
	if err := json.Unmarshal([]byte(code), &s.Code); err != nil {
		return s, err
	}
	return s, nil
}

// CreateSearch stores a proposed search (state "proposed" unless the caller sets another). A caller that has to
// register state under the id before the row becomes visible sets s.ID (like the approval, issue #154).
func (s *Store) CreateSearch(ctx context.Context, in Search) (Search, error) {
	if in.ID == "" {
		in.ID = newHexID("s")
	}
	if in.State == "" {
		in.State = SearchProposed
	}
	code, err := json.Marshal(in.Code)
	if err != nil {
		return Search{}, err
	}
	return scanSearch(s.pool.QueryRow(ctx, `
INSERT INTO searches (id, chat_id, title, state, path, objective, space, budget, base, code, holdout, note, session, approval_id, tool_call_id)
VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8::jsonb,$9::jsonb,$10::jsonb,$11::jsonb,$12,$13,$14,$15) RETURNING `+searchCols,
		in.ID, in.ChatID, in.Title, in.State, in.Path, jsonOr(in.Objective, "{}"), jsonOr(in.Space, "{}"), jsonOr(in.Budget, "{}"),
		jsonOr(in.Base, "{}"), string(code), jsonOr(in.Holdout, "{}"), in.Note, in.Session, in.ApprovalID, in.ToolCallID))
}

// GetSearch returns one search of a chat; ErrNotFound for an unknown id or another chat's search.
func (s *Store) GetSearch(ctx context.Context, chatID, id string) (Search, error) {
	if !isUUID(chatID) {
		return Search{}, ErrNotFound
	}
	return scanSearch(s.pool.QueryRow(ctx, `SELECT `+searchCols+` FROM searches WHERE id=$1 AND chat_id=$2`, id, chatID))
}

// RunningSearch returns the running search of a chat; ErrNotFound when there is none.
func (s *Store) RunningSearch(ctx context.Context, chatID string) (Search, error) {
	if !isUUID(chatID) {
		return Search{}, ErrNotFound
	}
	return scanSearch(s.pool.QueryRow(ctx, `SELECT `+searchCols+` FROM searches WHERE chat_id=$1 AND state='running'`, chatID))
}

// ListSearches returns the searches of a chat, oldest first.
func (s *Store) ListSearches(ctx context.Context, chatID string) ([]Search, error) {
	if !isUUID(chatID) {
		return []Search{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+searchCols+` FROM searches WHERE chat_id=$1 ORDER BY created_at, id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Search{}
	for rows.Next() {
		x, err := scanSearch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// OpenSearches returns every running search of every chat (the watcher's tick looks at their time budget).
func (s *Store) OpenSearches(ctx context.Context) ([]Search, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+searchCols+` FROM searches WHERE state='running' ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Search{}
	for rows.Next() {
		x, err := scanSearch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DecideSearch decides a proposed search: "running" starts it (started_at now, ends_by as given), "rejected" and
// "expired" close it. It takes effect only while the state is "proposed"; ok reports whether it did, otherwise the
// existing search is returned. A second running search in the chat gives ErrSearchRunning.
func (s *Store) DecideSearch(ctx context.Context, id, state string, endsBy *time.Time) (Search, bool, error) {
	var row pgx.Row
	switch state {
	case SearchRunning:
		row = s.pool.QueryRow(ctx, `UPDATE searches SET state='running', started_at=now(), ends_by=$2
WHERE id=$1 AND state='proposed' RETURNING `+searchCols, id, endsBy)
	case SearchRejected, SearchExpired:
		row = s.pool.QueryRow(ctx, `UPDATE searches SET state=$2, ended_at=now()
WHERE id=$1 AND state='proposed' RETURNING `+searchCols, id, state)
	default:
		return Search{}, false, errors.New("search: state " + state + " is not a decision")
	}
	x, err := scanSearch(row)
	if err == nil {
		return x, true, nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Search{}, false, ErrSearchRunning
	}
	if !errors.Is(err, ErrNotFound) {
		return Search{}, false, err
	}
	x, err = s.searchByID(ctx, id)
	return x, false, err
}

func (s *Store) searchByID(ctx context.Context, id string) (Search, error) {
	return scanSearch(s.pool.QueryRow(ctx, `SELECT `+searchCols+` FROM searches WHERE id=$1`, id))
}

// EndSearch ends a running search with a reason and, if there is one, the winner (the idx of the trial). It takes
// effect only from "running"; ok reports whether it did.
func (s *Store) EndSearch(ctx context.Context, id, reason string, winnerIdx *int) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE searches SET state='ended', end_reason=$2, winner_idx=$3, ended_at=now()
WHERE id=$1 AND state='running'`, id, reason, winnerIdx)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateSearchBudget replaces the budget of a running search (the user's route). ok is false once it ended.
func (s *Store) UpdateSearchBudget(ctx context.Context, id string, budget json.RawMessage) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE searches SET budget=$2::jsonb WHERE id=$1 AND state='running'`, id, string(budget))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetSearchCode stores the approved code of a notebook search (the first one with the search, a new version
// after the user approved a change).
func (s *Store) SetSearchCode(ctx context.Context, id string, code SearchCode) error {
	b, err := json.Marshal(code)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE searches SET code=$2::jsonb WHERE id=$1`, id, string(b))
	return err
}

// SetSearchHoldout stores the state of the hold-out test of a search (available, eval dataset, final_state, check).
func (s *Store) SetSearchHoldout(ctx context.Context, id string, holdout json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `UPDATE searches SET holdout=$2::jsonb WHERE id=$1`, id, jsonOr(holdout, "{}"))
	return err
}

// SetSearchBase stores the base of a template search again (the configuration read when the approval is decided).
func (s *Store) SetSearchBase(ctx context.Context, id string, base json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `UPDATE searches SET base=$2::jsonb WHERE id=$1`, id, jsonOr(base, "{}"))
	return err
}

// IncSearchAutoTurns counts one more turn the agent was woken for by the search and returns the count.
func (s *Store) IncSearchAutoTurns(ctx context.Context, id string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `UPDATE searches SET auto_turns = auto_turns + 1 WHERE id=$1 RETURNING auto_turns`, id).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// SetSearchApproval records the approval a search was proposed with.
func (s *Store) SetSearchApproval(ctx context.Context, id, approvalID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE searches SET approval_id=$2 WHERE id=$1`, id, approvalID)
	return err
}

// --- trials ---

const trialCols = `id, chat_id::text, COALESCE(search_id,''), seq, idx, path, state, params::text, note, ref, job_id, code_version, source,
epoch, epochs, score, score_epoch, COALESCE(gpu::text,''), figures, gpu_available, error, stopped_by, declared_at, started_at, ended_at`

func scanTrial(row pgx.Row) (Trial, error) {
	var t Trial
	var params, gpu string
	err := row.Scan(&t.ID, &t.ChatID, &t.SearchID, &t.Seq, &t.Idx, &t.Path, &t.State, &params, &t.Note, &t.Ref, &t.JobID, &t.CodeVersion,
		&t.Source, &t.Epoch, &t.Epochs, &t.Score, &t.ScoreEpoch, &gpu, &t.Figures, &t.GPUAvailable, &t.Error, &t.StoppedBy, &t.DeclaredAt, &t.StartedAt, &t.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.Params = json.RawMessage(params)
	if gpu != "" {
		t.GPU = json.RawMessage(gpu)
	}
	return t, nil
}

// DeclareTrial stores a new trial in state "declared" and numbers it: seq within the chat, idx within the search
// (0 for a single run, when SearchID is empty). The caller's ID, if any, is kept.
func (s *Store) DeclareTrial(ctx context.Context, in Trial) (Trial, error) {
	if in.ID == "" {
		in.ID = newHexID("t")
	}
	if in.State == "" {
		in.State = TrialDeclared
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		t, err := scanTrial(s.pool.QueryRow(ctx, `
INSERT INTO experiment_trials (id, chat_id, search_id, seq, idx, path, state, params, note, ref, job_id, code_version, source)
VALUES ($1, $2, NULLIF($3,''),
  (SELECT COALESCE(max(seq),0)+1 FROM experiment_trials WHERE chat_id=$2),
  CASE WHEN $3 = '' THEN 0 ELSE (SELECT COALESCE(max(idx),0)+1 FROM experiment_trials WHERE search_id=$3) END,
  $4, $5, $6::jsonb, $7, $8, $9, $10, $11) RETURNING `+trialCols,
			in.ID, in.ChatID, in.SearchID, in.Path, in.State, jsonOr(in.Params, "{}"), in.Note, in.Ref, in.JobID, in.CodeVersion, in.Source))
		if err == nil {
			return t, nil
		}
		// Two trials declared at once can draw the same number: try again.
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23505" {
			return Trial{}, err
		}
		lastErr = err
	}
	return Trial{}, lastErr
}

// GetTrial returns one trial of a chat; ErrNotFound for an unknown id or another chat's trial.
func (s *Store) GetTrial(ctx context.Context, chatID, id string) (Trial, error) {
	if !isUUID(chatID) {
		return Trial{}, ErrNotFound
	}
	return scanTrial(s.pool.QueryRow(ctx, `SELECT `+trialCols+` FROM experiment_trials WHERE id=$1 AND chat_id=$2`, id, chatID))
}

// StartTrial moves a declared trial to "running" and records the run (ref) and the observed job. ok is false when
// the trial is not "declared" any more.
func (s *Store) StartTrial(ctx context.Context, id, ref, jobID string) (bool, error) {
	return s.StartTrialAt(ctx, id, ref, jobID, time.Now())
}

// StartTrialAt is StartTrial with the start given (issue #194: a single trial starts when its job did).
func (s *Store) StartTrialAt(ctx context.Context, id, ref, jobID string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET state='running', ref=$2, job_id=$3, started_at=$4
WHERE id=$1 AND state='declared'`, id, ref, jobID, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinishTrial ends a trial that is declared or running with a final state (done, failed, stopped, refused) and,
// for a failure, the reason. ok is false for a trial that already ended.
func (s *Store) FinishTrial(ctx context.Context, id, state, errText string) (bool, error) {
	switch state {
	case TrialDone, TrialFailed, TrialStopped, TrialRefused:
	default:
		return false, errors.New("trial: " + state + " is not a final state")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET state=$2, error=$3, ended_at=now()
WHERE id=$1 AND state IN ('declared','running')`, id, state, errText)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetTrialStoppedBy records who stopped a trial (StoppedByUser, StoppedByAgent or "budget_time" and the like).
func (s *Store) SetTrialStoppedBy(ctx context.Context, id, by string) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET stopped_by=$2 WHERE id=$1`, id, by)
	return err
}

// SetTrialProgress stores what the last poll found: the epoch, the number of epochs, the score of the objective
// (with the epoch it belongs to) and the last GPU sample. A nil value leaves the column as it is.
func (s *Store) SetTrialProgress(ctx context.Context, id string, epoch, epochs *int, score *float64, scoreEpoch *int, gpu json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET
epoch = COALESCE($2, epoch), epochs = COALESCE($3, epochs), score = COALESCE($4, score),
score_epoch = COALESCE($5, score_epoch), gpu = COALESCE(NULLIF($6,'')::jsonb, gpu) WHERE id=$1`,
		id, epoch, epochs, score, scoreEpoch, string(gpu))
	return err
}

// SetTrialFigures records what the run of a trial writes: figures "none" or "ok" and whether it sampled a GPU.
func (s *Store) SetTrialFigures(ctx context.Context, id, figures string, gpuAvailable bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET figures=$2, gpu_available=$3 WHERE id=$1`, id, figures, gpuAvailable)
	return err
}

// TrialByJob returns the trial of an observed job; ErrNotFound when the job belongs to none.
func (s *Store) TrialByJob(ctx context.Context, jobID string) (Trial, error) {
	if jobID == "" {
		return Trial{}, ErrNotFound
	}
	return scanTrial(s.pool.QueryRow(ctx, `SELECT `+trialCols+` FROM experiment_trials WHERE job_id=$1`, jobID))
}

// TrialByRef returns the trial that was declared for a run (a training container or a notebook run) of a chat;
// ErrNotFound when none was.
func (s *Store) TrialByRef(ctx context.Context, chatID, path, ref string) (Trial, error) {
	if !isUUID(chatID) || ref == "" {
		return Trial{}, ErrNotFound
	}
	return scanTrial(s.pool.QueryRow(ctx, `SELECT `+trialCols+` FROM experiment_trials WHERE chat_id=$1 AND path=$2 AND ref=$3
ORDER BY seq DESC LIMIT 1`, chatID, path, ref))
}

// SetTrialRef records the run a declared trial is going to use (the clone the agent made), before it starts.
func (s *Store) SetTrialRef(ctx context.Context, id, ref string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET ref=$2 WHERE id=$1 AND state='declared'`, id, ref)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimTrialClone reserves the copy of the base container for a declared trial that has none yet (its ref is
// empty): the first call gets true, a second one for the same trial gets false. ReleaseTrialClone gives it back.
func (s *Store) ClaimTrialClone(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET ref='-' WHERE id=$1 AND state='declared' AND ref=''`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseTrialClone gives back a copy that was reserved but not made.
func (s *Store) ReleaseTrialClone(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET ref='' WHERE id=$1 AND state='declared' AND ref='-'`, id)
	return err
}

// ClaimTrialStart moves a declared trial to "running" for a call that starts it (the start of the copy, the
// notebook run) and reserves it: the first call gets true, a second one false. ref must be the ref the trial has
// ("" for a notebook trial, whose run id is known after the start). ReleaseTrialStart gives it back when the call
// failed; SetTrialJob completes it when it worked.
func (s *Store) ClaimTrialStart(ctx context.Context, id, ref string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET state='running', started_at=now()
WHERE id=$1 AND state='declared' AND ref=$2`, id, ref)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseTrialStart undoes ClaimTrialStart for a trial whose start did not happen (it has no job yet).
func (s *Store) ReleaseTrialStart(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET state='declared', started_at=NULL WHERE id=$1 AND state='running' AND job_id=''`, id)
	return err
}

// SetTrialJob records the run and the observed job of a trial that is running; an empty ref leaves the ref as it is.
func (s *Store) SetTrialJob(ctx context.Context, id, ref, jobID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET ref=COALESCE(NULLIF($2,''), ref), job_id=$3 WHERE id=$1 AND state='running'`, id, ref, jobID)
	return err
}

// SetTrialCodeVersion records the version of the code a trial ran with (a change the user approved).
func (s *Store) SetTrialCodeVersion(ctx context.Context, id string, version int) error {
	_, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET code_version=$2 WHERE id=$1`, id, version)
	return err
}

// ListTrials returns every trial of a chat in the order they were declared (the book of trials).
func (s *Store) ListTrials(ctx context.Context, chatID string) ([]Trial, error) {
	if !isUUID(chatID) {
		return []Trial{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+trialCols+` FROM experiment_trials WHERE chat_id=$1 ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trial{}
	for rows.Next() {
		t, err := scanTrial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddTrialEpochs stores epoch lines; a line of an epoch that is already there replaces it. Above MaxTrialEpochs
// the older half is thinned out 2:1.
func (s *Store) AddTrialEpochs(ctx context.Context, trialID string, lines []TrialEpoch) error {
	for _, l := range lines {
		if _, err := s.pool.Exec(ctx, `INSERT INTO trial_epochs (trial_id, epoch, at, data) VALUES ($1,$2,$3,$4::jsonb)
ON CONFLICT (trial_id, epoch) DO UPDATE SET at=EXCLUDED.at, data=EXCLUDED.data`, trialID, l.Epoch, l.At, jsonOr(l.Data, "{}")); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM trial_epochs e USING (
  SELECT epoch, row_number() OVER (ORDER BY epoch) AS rn, count(*) OVER () AS n FROM trial_epochs WHERE trial_id=$1
) r WHERE e.trial_id=$1 AND e.epoch=r.epoch AND r.n > $2 AND r.rn <= r.n / 2 AND r.rn % 2 = 0`, trialID, MaxTrialEpochs)
	return err
}

// AddTrialGPU stores GPU samples; a sample at a time that is already there replaces it. Above MaxTrialGPU the
// older half is thinned out 2:1.
func (s *Store) AddTrialGPU(ctx context.Context, trialID string, samples []TrialGPU) error {
	for _, g := range samples {
		if _, err := s.pool.Exec(ctx, `INSERT INTO trial_gpu (trial_id, at, util, mem_used_mb, mem_total_mb) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (trial_id, at) DO UPDATE SET util=EXCLUDED.util, mem_used_mb=EXCLUDED.mem_used_mb, mem_total_mb=EXCLUDED.mem_total_mb`,
			trialID, g.At, g.Util, g.MemUsedMB, g.MemTotalMB); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM trial_gpu e USING (
  SELECT at, row_number() OVER (ORDER BY at) AS rn, count(*) OVER () AS n FROM trial_gpu WHERE trial_id=$1
) r WHERE e.trial_id=$1 AND e.at=r.at AND r.n > $2 AND r.rn <= r.n / 2 AND r.rn % 2 = 0`, trialID, MaxTrialGPU)
	return err
}

// TrialSeries returns the epoch lines and the GPU samples of a trial, oldest first.
func (s *Store) TrialSeries(ctx context.Context, trialID string) ([]TrialEpoch, []TrialGPU, error) {
	rows, err := s.pool.Query(ctx, `SELECT trial_id, epoch, at, data::text FROM trial_epochs WHERE trial_id=$1 ORDER BY epoch`, trialID)
	if err != nil {
		return nil, nil, err
	}
	epochs := []TrialEpoch{}
	for rows.Next() {
		var e TrialEpoch
		var data string
		if err := rows.Scan(&e.TrialID, &e.Epoch, &e.At, &data); err != nil {
			rows.Close()
			return nil, nil, err
		}
		e.Data = json.RawMessage(data)
		epochs = append(epochs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	grows, err := s.pool.Query(ctx, `SELECT trial_id, at, COALESCE(util,0), COALESCE(mem_used_mb,0), COALESCE(mem_total_mb,0)
FROM trial_gpu WHERE trial_id=$1 ORDER BY at`, trialID)
	if err != nil {
		return nil, nil, err
	}
	defer grows.Close()
	gpu := []TrialGPU{}
	for grows.Next() {
		var g TrialGPU
		if err := grows.Scan(&g.TrialID, &g.At, &g.Util, &g.MemUsedMB, &g.MemTotalMB); err != nil {
			return nil, nil, err
		}
		gpu = append(gpu, g)
	}
	return epochs, gpu, grows.Err()
}

// AddSearchEvent appends a line to the record of a search.
func (s *Store) AddSearchEvent(ctx context.Context, e SearchEvent) (SearchEvent, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO search_events (search_id, kind, data) VALUES ($1,$2,$3::jsonb) RETURNING id, at`,
		e.SearchID, e.Kind, jsonOr(e.Data, "{}")).Scan(&e.ID, &e.At)
	return e, err
}

// SearchEvents returns the record of a search, oldest first.
func (s *Store) SearchEvents(ctx context.Context, searchID string) ([]SearchEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, search_id, at, kind, data::text FROM search_events WHERE search_id=$1 ORDER BY id`, searchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SearchEvent{}
	for rows.Next() {
		var e SearchEvent
		var data string
		if err := rows.Scan(&e.ID, &e.SearchID, &e.At, &e.Kind, &data); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

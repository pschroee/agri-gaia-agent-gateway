package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// States of a background task (table background_tasks).
const (
	BgRunning   = "running"
	BgExited    = "exited"    // the command has ended (exit code)
	BgFailed    = "failed"    // not started or aborted with an error
	BgTimeout   = "timeout"   // timeout of the call reached
	BgStopped   = "stopped"   // bg_stop by the agent or stop in the UI (stopped_by)
	BgLost      = "lost"      // execution sandbox or connection gone, orchestrator restarted
	BgSuspended = "suspended" // ended together with the sandbox when the chat went idle
	BgClosed    = "closed"    // only in old rows: chat closed (closing no longer exists since 2026-09-30)
)

// BackgroundTask is a background task (bash with run_in_background). The orchestrator
// starts and follows it itself; command, output (checksum, excerpt) and end are proven
// like every execution in tool_executions.
type BackgroundTask struct {
	ID            string     `json:"id"` // bg-<seq>
	Seq           int        `json:"seq"`
	ChatID        string     `json:"chat_id"`
	SlotID        string     `json:"slot_id"`
	Session       string     `json:"session"` // "main" or run of the subagent
	ToolCallID    string     `json:"tool_call_id"`
	Command       string     `json:"command"`
	Cwd           string     `json:"cwd,omitempty"`
	LogPath       string     `json:"log_path"`
	State         string     `json:"state"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Error         string     `json:"error,omitempty"`
	StoppedBy     string     `json:"stopped_by,omitempty"` // agent, user
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	OutputBytes   int64      `json:"output_bytes"`
	OutputLines   int64      `json:"output_lines"`
	OutputExcerpt string     `json:"output_excerpt,omitempty"`
	OutputSHA256  string     `json:"output_sha256,omitempty"`
	// Tail: the last lines of the output (at most a few KiB), for display and the note.
	Tail          string     `json:"tail,omitempty"`
	NotifiedAt    *time.Time `json:"notified_at,omitempty"`
	Woke          bool       `json:"woke,omitempty"`
	NoticePending bool       `json:"notice_pending,omitempty"`
}

// BgID: ID of a background task within the chat.
func BgID(seq int) string { return fmt.Sprintf("bg-%d", seq) }

// ParseBgID reads "bg-<n>"; 0 if it is not a valid ID.
func ParseBgID(id string) int {
	rest, ok := strings.CutPrefix(strings.TrimSpace(id), "bg-")
	if !ok || rest == "" || len(rest) > 9 || rest[0] == '0' {
		return 0
	}
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

const bgCols = `seq, chat_id::text, slot_id, session, tool_call_id, command, cwd, log_path, state, exit_code, error, stopped_by,
  started_at, ended_at, output_bytes, output_lines, output_excerpt, output_sha256, tail, notified_at, woke, notice_pending`

func scanBg(rows pgx.Rows) ([]BackgroundTask, error) {
	defer rows.Close()
	out := []BackgroundTask{}
	for rows.Next() {
		var t BackgroundTask
		if err := rows.Scan(&t.Seq, &t.ChatID, &t.SlotID, &t.Session, &t.ToolCallID, &t.Command, &t.Cwd, &t.LogPath, &t.State, &t.ExitCode,
			&t.Error, &t.StoppedBy, &t.StartedAt, &t.EndedAt, &t.OutputBytes, &t.OutputLines, &t.OutputExcerpt, &t.OutputSHA256, &t.Tail,
			&t.NotifiedAt, &t.Woke, &t.NoticePending); err != nil {
			return nil, err
		}
		t.ID = BgID(t.Seq)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateBackgroundTask creates a running task with the chat's next number. LogPath
// builds logPath from the number (it is only known after the insert).
func (s *Store) CreateBackgroundTask(ctx context.Context, t BackgroundTask, logPath func(seq int) string) (BackgroundTask, error) {
	if t.Session == "" {
		t.Session = "main"
	}
	t.Command, t.Cwd, t.ToolCallID, t.Session = noNUL(t.Command), noNUL(t.Cwd), noNUL(t.ToolCallID), noNUL(t.Session)
	for attempt := 0; ; attempt++ {
		rows, err := s.pool.Query(ctx, `
INSERT INTO background_tasks (chat_id, seq, slot_id, session, tool_call_id, command, cwd, state, started_at)
SELECT $1, COALESCE(max(seq), 0) + 1, $2, $3, $4, $5, $6, 'running', now() FROM background_tasks WHERE chat_id = $1
RETURNING `+bgCols, t.ChatID, t.SlotID, t.Session, t.ToolCallID, t.Command, t.Cwd)
		if err != nil {
			return BackgroundTask{}, err
		}
		list, err := scanBg(rows)
		var pe *pgconn.PgError
		if err != nil && errors.As(err, &pe) && pe.Code == "23505" && attempt < 5 {
			continue // created concurrently: next number
		}
		if err != nil {
			return BackgroundTask{}, err
		}
		if len(list) != 1 {
			return BackgroundTask{}, errors.New("background task: no row")
		}
		created := list[0]
		if logPath != nil {
			created.LogPath = logPath(created.Seq)
			if _, err := s.pool.Exec(ctx, `UPDATE background_tasks SET log_path=$3 WHERE chat_id=$1 AND seq=$2`, created.ChatID, created.Seq, created.LogPath); err != nil {
				return BackgroundTask{}, err
			}
		}
		return created, nil
	}
}

// FinishBackgroundTask records the end of a running task. false: it had already ended
// (e.g. marked in advance when the chat went idle); then the earlier state stays, and only the
// output details are added.
func (s *Store) FinishBackgroundTask(ctx context.Context, t BackgroundTask) (bool, error) {
	ended := time.Now()
	if t.EndedAt != nil {
		ended = *t.EndedAt
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE background_tasks SET state=$3, exit_code=$4, error=$5, stopped_by=$6, ended_at=$7, output_bytes=$8, output_lines=$9,
  output_excerpt=$10, output_sha256=$11, tail=$12
WHERE chat_id=$1 AND seq=$2 AND state='running'`,
		t.ChatID, t.Seq, t.State, t.ExitCode, noNUL(t.Error), t.StoppedBy, ended, t.OutputBytes, t.OutputLines,
		noNUL(t.OutputExcerpt), t.OutputSHA256, noNUL(t.Tail))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	// Already ended: add the output (which the orchestrator kept reading until the end).
	_, err = s.pool.Exec(ctx, `
UPDATE background_tasks SET output_bytes=$3, output_lines=$4, output_excerpt=$5, output_sha256=$6, tail=$7
WHERE chat_id=$1 AND seq=$2 AND output_sha256=''`,
		t.ChatID, t.Seq, t.OutputBytes, t.OutputLines, noNUL(t.OutputExcerpt), t.OutputSHA256, noNUL(t.Tail))
	return false, err
}

// EndRunningBackground ends all running tasks of a chat with state (suspended, closed,
// lost) and reason; notice: tell the agent once with the next request.
func (s *Store) EndRunningBackground(ctx context.Context, chatID, state, reason string, notice bool) ([]BackgroundTask, error) {
	if !isUUID(chatID) {
		return []BackgroundTask{}, nil
	}
	rows, err := s.pool.Query(ctx, `
UPDATE background_tasks SET state=$2, error=$3, ended_at=now(), notice_pending=$4
WHERE chat_id=$1 AND state='running' RETURNING `+bgCols, chatID, state, reason, notice)
	if err != nil {
		return nil, err
	}
	return scanBg(rows)
}

// EndAllRunningBackground: after a restart of the orchestrator no task is running anymore.
func (s *Store) EndAllRunningBackground(ctx context.Context, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE background_tasks SET state='lost', error=$1, ended_at=now(), notice_pending=true WHERE state='running'`, reason)
	return tag.RowsAffected(), err
}

func (s *Store) ListBackgroundTasks(ctx context.Context, chatID string) ([]BackgroundTask, error) {
	if !isUUID(chatID) {
		return []BackgroundTask{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+bgCols+` FROM background_tasks WHERE chat_id=$1 ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	return scanBg(rows)
}

func (s *Store) GetBackgroundTask(ctx context.Context, chatID string, seq int) (BackgroundTask, error) {
	if !isUUID(chatID) {
		return BackgroundTask{}, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+bgCols+` FROM background_tasks WHERE chat_id=$1 AND seq=$2`, chatID, seq)
	if err != nil {
		return BackgroundTask{}, err
	}
	list, err := scanBg(rows)
	if err != nil {
		return BackgroundTask{}, err
	}
	if len(list) == 0 {
		return BackgroundTask{}, ErrNotFound
	}
	return list[0], nil
}

// MarkBackgroundNotified records that the agent received the note (woke: it started a
// new turn).
func (s *Store) MarkBackgroundNotified(ctx context.Context, chatID string, seq int, woke bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE background_tasks SET notified_at=now(), woke=$3 WHERE chat_id=$1 AND seq=$2`, chatID, seq, woke)
	return err
}

// MarkBackgroundWoke: the note of this task started a turn without the user
// (wake-up). Wake-ups are counted in chat_turns (WakesSince).
func (s *Store) MarkBackgroundWoke(ctx context.Context, chatID string, seq int) error {
	_, err := s.pool.Exec(ctx, `UPDATE background_tasks SET woke=true WHERE chat_id=$1 AND seq=$2`, chatID, seq)
	return err
}

// BackgroundNotices: tasks ended while idling that the agent does not know about yet.
func (s *Store) BackgroundNotices(ctx context.Context, chatID string) ([]BackgroundTask, error) {
	if !isUUID(chatID) {
		return []BackgroundTask{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+bgCols+` FROM background_tasks WHERE chat_id=$1 AND notice_pending ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	return scanBg(rows)
}

// ClearBackgroundNotices: the agent has been told.
func (s *Store) ClearBackgroundNotices(ctx context.Context, chatID string, seqs []int) error {
	if len(seqs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE background_tasks SET notice_pending=false WHERE chat_id=$1 AND seq = ANY($2::int[])`, chatID, seqs)
	return err
}

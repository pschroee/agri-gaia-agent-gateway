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

// Zustände einer Hintergrundaufgabe (Tabelle background_tasks).
const (
	BgRunning   = "running"
	BgExited    = "exited"    // der Befehl ist geendet (Exit-Code)
	BgFailed    = "failed"    // nicht gestartet oder mit Fehler abgebrochen
	BgTimeout   = "timeout"   // Zeitgrenze des Aufrufs erreicht
	BgStopped   = "stopped"   // bg_stop des Agenten oder Stopp in der UI (stopped_by)
	BgLost      = "lost"      // Ausführungs-Sandbox oder Verbindung weg, Orchestrator neu gestartet
	BgSuspended = "suspended" // beim Ruhen des Chats mit der Sandbox beendet
	BgClosed    = "closed"    // nur in alten Zeilen: Chat beendet (Beenden gibt es seit 30.09.2026 nicht mehr)
)

// BackgroundTask ist eine Hintergrundaufgabe (bash mit run_in_background). Der Orchestrator
// startet und verfolgt sie selbst; Befehl, Ausgabe (Prüfsumme, Auszug) und Ende sind belegt
// wie jede Ausführung in tool_executions.
type BackgroundTask struct {
	ID            string     `json:"id"` // bg-<seq>
	Seq           int        `json:"seq"`
	ChatID        string     `json:"chat_id"`
	SlotID        string     `json:"slot_id"`
	Session       string     `json:"session"` // "main" oder Lauf des Subagenten
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
	// Tail: die letzten Zeilen der Ausgabe (höchstens einige KiB), für Anzeige und Meldung.
	Tail          string     `json:"tail,omitempty"`
	NotifiedAt    *time.Time `json:"notified_at,omitempty"`
	Woke          bool       `json:"woke,omitempty"`
	NoticePending bool       `json:"notice_pending,omitempty"`
}

// BgID: Kennung einer Hintergrundaufgabe im Chat.
func BgID(seq int) string { return fmt.Sprintf("bg-%d", seq) }

// ParseBgID liest „bg-<n>“; 0, wenn es keine gültige Kennung ist.
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

// CreateBackgroundTask legt eine laufende Aufgabe mit der nächsten Nummer des Chats an. LogPath
// bildet logPath aus der Nummer (sie steht erst nach dem Einfügen fest).
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
			continue // gleichzeitig angelegt: nächste Nummer
		}
		if err != nil {
			return BackgroundTask{}, err
		}
		if len(list) != 1 {
			return BackgroundTask{}, errors.New("Hintergrundaufgabe: keine Zeile")
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

// FinishBackgroundTask trägt das Ende einer laufenden Aufgabe ein. false: Sie war schon beendet
// (etwa beim Ruhen des Chats vorab markiert); dann bleibt der frühere Zustand stehen, und nur die
// Angaben zur Ausgabe werden ergänzt.
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
	// Schon beendet: die Ausgabe (die der Orchestrator bis zuletzt mitgelesen hat) nachtragen.
	_, err = s.pool.Exec(ctx, `
UPDATE background_tasks SET output_bytes=$3, output_lines=$4, output_excerpt=$5, output_sha256=$6, tail=$7
WHERE chat_id=$1 AND seq=$2 AND output_sha256=''`,
		t.ChatID, t.Seq, t.OutputBytes, t.OutputLines, noNUL(t.OutputExcerpt), t.OutputSHA256, noNUL(t.Tail))
	return false, err
}

// EndRunningBackground beendet alle laufenden Aufgaben eines Chats mit state (suspended, closed,
// lost) und reason; notice: dem Agenten beim nächsten Auftrag einmal sagen.
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

// EndAllRunningBackground: nach einem Neustart des Orchestrators läuft keine Aufgabe mehr.
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

// MarkBackgroundNotified hält fest, dass der Agent die Meldung bekommen hat (woke: sie hat einen
// neuen Durchgang gestartet).
func (s *Store) MarkBackgroundNotified(ctx context.Context, chatID string, seq int, woke bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE background_tasks SET notified_at=now(), woke=$3 WHERE chat_id=$1 AND seq=$2`, chatID, seq, woke)
	return err
}

// MarkBackgroundWoke: Die Meldung dieser Aufgabe hat einen Durchgang ohne Nutzer gestartet
// (Weckruf). Gezählt werden Weckrufe in chat_turns (WakesSince).
func (s *Store) MarkBackgroundWoke(ctx context.Context, chatID string, seq int) error {
	_, err := s.pool.Exec(ctx, `UPDATE background_tasks SET woke=true WHERE chat_id=$1 AND seq=$2`, chatID, seq)
	return err
}

// BackgroundNotices: beim Ruhen beendete Aufgaben, die der Agent noch nicht kennt.
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

// ClearBackgroundNotices: dem Agenten gesagt.
func (s *Store) ClearBackgroundNotices(ctx context.Context, chatID string, seqs []int) error {
	if len(seqs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE background_tasks SET notice_pending=false WHERE chat_id=$1 AND seq = ANY($2::int[])`, chatID, seqs)
	return err
}

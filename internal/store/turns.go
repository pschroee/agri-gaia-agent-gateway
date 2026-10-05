package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Turns (Review 3, H1/H2): every request to pi is a turn with a trigger and an origin.
// For the evaluation this proves what the user asked for and what the orchestrator delivered on its
// own; the manager uses it to count wake-ups per hour and consecutive turns without the user.

// Trigger of a turn.
const (
	TriggerUser  = "user"  // the user sent something (message, "Send now")
	TriggerQueue = "queue" // delivered at the end of a run, with at least one user message among it
	TriggerWake  = "wake"  // only orchestrator notes, without the user's involvement (wake-up)
)

// Origin of a request to pi.
const (
	OriginUser   = "user"   // only text from the user
	OriginSystem = "system" // only orchestrator notes
	OriginMixed  = "mixed"  // both
)

// Source is a part of a request, in order.
type Source struct {
	Kind    string   `json:"kind"`               // QueueUser or QueueSystem
	Type    string   `json:"type,omitempty"`     // for system: NoteBackground, NoteSandbox, NoteLanguage
	Refs    []string `json:"refs,omitempty"`     // for system: affected tasks
	QueueID string   `json:"queue_id,omitempty"` // queue entry, if enqueued
	// Marker: marker of the fence around the data from the sandbox (only system, if there is data).
	Marker string `json:"marker,omitempty"`
}

// Turn is a turn.
type Turn struct {
	ID        int64     `json:"id"`
	ChatID    string    `json:"chat_id"`
	Trigger   string    `json:"trigger"`
	Origin    string    `json:"origin"`
	Sources   []Source  `json:"sources"`
	QueueIDs  []string  `json:"queue_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateTurn creates a turn (before the request to pi; if that fails, DeleteTurn).
func (s *Store) CreateTurn(ctx context.Context, chatID, trigger, origin string, sources []Source, queueIDs []string) (int64, error) {
	if sources == nil {
		sources = []Source{}
	}
	if queueIDs == nil {
		queueIDs = []string{}
	}
	src, _ := json.Marshal(sources)
	qs, _ := json.Marshal(queueIDs)
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO chat_turns (chat_id, trigger, origin, sources, queue_ids) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		chatID, trigger, origin, src, qs).Scan(&id)
	return id, err
}

// DeleteTurn withdraws a turn whose request pi did not accept.
func (s *Store) DeleteTurn(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM chat_turns WHERE id=$1`, id)
	return err
}

// FirstTurnLanguage returns the chat's preferred language as long as it has no turn yet;
// otherwise (and if not given) "".
func (s *Store) FirstTurnLanguage(ctx context.Context, chatID string) (string, error) {
	if !isUUID(chatID) {
		return "", nil
	}
	var lang string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(language, '') FROM chats c WHERE id=$1 AND NOT EXISTS (SELECT 1 FROM chat_turns t WHERE t.chat_id=c.id)`, chatID).Scan(&lang)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return lang, err
}

// Turns returns a chat's turns in order.
func (s *Store) Turns(ctx context.Context, chatID string) ([]Turn, error) {
	out := []Turn{}
	if !isUUID(chatID) {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, chat_id::text, trigger, origin, sources, queue_ids, created_at FROM chat_turns WHERE chat_id=$1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Turn
		var src, qs []byte
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Trigger, &t.Origin, &src, &qs, &t.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(src, &t.Sources)
		_ = json.Unmarshal(qs, &t.QueueIDs)
		out = append(out, t)
	}
	return out, rows.Err()
}

// WakesSince counts a chat's wake-ups (turns without the user's involvement) since since.
func (s *Store) WakesSince(ctx context.Context, chatID string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM chat_turns WHERE chat_id=$1 AND trigger=$2 AND created_at >= $3`, chatID, TriggerWake, since).Scan(&n)
	return n, err
}

// AutoTurnsInRow counts the wake-ups since the last turn the user was involved in.
func (s *Store) AutoTurnsInRow(ctx context.Context, chatID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM chat_turns
WHERE chat_id=$1 AND id > COALESCE((SELECT max(id) FROM chat_turns WHERE chat_id=$1 AND trigger<>$2), 0)`, chatID, TriggerWake).Scan(&n)
	return n, err
}

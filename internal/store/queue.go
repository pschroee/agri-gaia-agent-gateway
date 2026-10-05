package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrDelivered: the queue entry has already been delivered to pi.
var ErrDelivered = errors.New("message has already been delivered")

// QueueEntry is an enqueued message (queue per chat).
type QueueEntry struct {
	ID          string    `json:"id"`
	ChatID      string    `json:"chat_id"`
	Text        string    `json:"text"`
	Attachments []string  `json:"attachments"`
	CreatedAt   time.Time `json:"created_at"`
	// Kind: "user" (message from the user) or "system" (orchestrator note, such as the
	// end of a background task).
	Kind string `json:"kind"`
	// Note and Refs only for system entries: kind of note (NoteBackground, NoteSandbox) and the
	// affected tasks (bg-3). Text is then the first line (built by the orchestrator) and
	// below it what comes from the sandbox (command, output); see chat.BackgroundNote.
	Note string   `json:"note,omitempty"`
	Refs []string `json:"refs,omitempty"`
}

// Kinds of a queue entry.
const (
	QueueUser   = "user"
	QueueSystem = "system"
)

// Kinds of an orchestrator note.
const (
	NoteBackground = "background" // end of a background task
	NoteSandbox    = "sandbox"    // tasks were ended with the previous sandbox
	NoteLanguage   = "language"   // the user's preferred language according to the browser (first request of the chat)
)

const queueCols = `id::text, chat_id::text, text, attachments, created_at, kind, note, refs`

func scanQueue(rows pgx.Rows) ([]QueueEntry, error) {
	defer rows.Close()
	out := []QueueEntry{}
	for rows.Next() {
		var e QueueEntry
		var att, refs []byte
		if err := rows.Scan(&e.ID, &e.ChatID, &e.Text, &att, &e.CreatedAt, &e.Kind, &e.Note, &refs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(refs, &e.Refs)
		if len(e.Refs) == 0 {
			e.Refs = nil
		}
		_ = json.Unmarshal(att, &e.Attachments)
		if e.Attachments == nil {
			e.Attachments = []string{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Enqueue enqueues a message from the user.
func (s *Store) Enqueue(ctx context.Context, chatID, text string, attachments []string) (QueueEntry, error) {
	return s.EnqueueKind(ctx, chatID, QueueUser, text, attachments)
}

// EnqueueKind enqueues an entry of the given kind (QueueUser, QueueSystem).
func (s *Store) EnqueueKind(ctx context.Context, chatID, kind, text string, attachments []string) (QueueEntry, error) {
	return s.enqueue(ctx, chatID, kind, text, attachments, "", nil)
}

// EnqueueSystem enqueues an orchestrator note (note: NoteBackground, NoteSandbox).
func (s *Store) EnqueueSystem(ctx context.Context, chatID, note string, refs []string, text string) (QueueEntry, error) {
	return s.enqueue(ctx, chatID, QueueSystem, text, nil, note, refs)
}

func (s *Store) enqueue(ctx context.Context, chatID, kind, text string, attachments []string, note string, refs []string) (QueueEntry, error) {
	if attachments == nil {
		attachments = []string{}
	}
	if refs == nil {
		refs = []string{}
	}
	if kind != QueueSystem {
		kind, note, refs = QueueUser, "", []string{}
	}
	att, _ := json.Marshal(attachments)
	rj, _ := json.Marshal(refs)
	rows, err := s.pool.Query(ctx, `INSERT INTO chat_queue (chat_id, text, attachments, kind, note, refs) VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+queueCols,
		chatID, text, att, kind, note, rj)
	if err != nil {
		return QueueEntry{}, err
	}
	es, err := scanQueue(rows)
	if err != nil {
		return QueueEntry{}, err
	}
	if len(es) != 1 {
		return QueueEntry{}, errors.New("enqueue: no row")
	}
	return es[0], nil
}

// ListQueue returns the open (not delivered) entries in order.
func (s *Store) ListQueue(ctx context.Context, chatID string) ([]QueueEntry, error) {
	if !isUUID(chatID) {
		return []QueueEntry{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+queueCols+` FROM chat_queue WHERE chat_id=$1 AND delivered_at IS NULL ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	return scanQueue(rows)
}

// ClaimQueue marks all open entries as delivered and returns them in order.
func (s *Store) ClaimQueue(ctx context.Context, chatID string) ([]QueueEntry, error) {
	rows, err := s.pool.Query(ctx, `
WITH c AS (UPDATE chat_queue SET delivered_at=now() WHERE chat_id=$1 AND delivered_at IS NULL RETURNING seq, `+queueCols+`)
SELECT `+queueCols+` FROM c ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	return scanQueue(rows)
}

// UnclaimQueue reverts a delivery (pi did not accept the request).
func (s *Store) UnclaimQueue(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE chat_queue SET delivered_at=NULL WHERE id = ANY($1::uuid[])`, ids)
	return err
}

// RemoveQueued removes an open entry. ErrNotFound: unknown; ErrDelivered: already delivered.
func (s *Store) RemoveQueued(ctx context.Context, chatID, id string) error {
	if !isUUID(chatID) || !isUUID(id) {
		return ErrNotFound
	}
	var delivered *time.Time
	err := s.pool.QueryRow(ctx, `SELECT delivered_at FROM chat_queue WHERE chat_id=$1 AND id=$2`, chatID, id).Scan(&delivered)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if delivered != nil {
		return ErrDelivered
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM chat_queue WHERE chat_id=$1 AND id=$2 AND delivered_at IS NULL`, chatID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDelivered // delivered between reading and deleting
	}
	return nil
}

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
	// Context: page context of a message from the user (only user entries; nil: none). It goes to
	// pi as a note of its own (NoteContext) before the text, never inside it.
	Context *PageContext `json:"context,omitempty"`
}

// PageContext is the platform page the user was on when sending, and the object opened or
// selected there (issue #13). The orchestrator checks it (chat.ParsePageContext) before it is
// stored; it tells the agent what the user refers to and grants no rights.
type PageContext struct {
	Page   string         `json:"page"`             // page id from a fixed list (chat.contextPages)
	Object *ContextObject `json:"object,omitempty"` // opened or selected object, if any
}

// ContextObject is an object of the platform: kind as the delegation names the resource
// (dataset, model, edge_device), id the platform's identifier, name as shown on the page.
type ContextObject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Kinds of a queue entry.
const (
	QueueUser   = "user"
	QueueSystem = "system"
)

// Kinds of an orchestrator note.
const (
	NoteBackground = "background"   // end of a background task
	NoteSandbox    = "sandbox"      // tasks were ended with the previous sandbox
	NoteLanguage   = "language"     // the user's preferred language according to the browser (first request of the chat)
	NoteContext    = "page_context" // page of the platform UI the user sent the message from (issue #13)
)

const queueCols = `id::text, chat_id::text, text, attachments, created_at, kind, note, refs, context`

func scanQueue(rows pgx.Rows) ([]QueueEntry, error) {
	defer rows.Close()
	out := []QueueEntry{}
	for rows.Next() {
		var e QueueEntry
		var att, refs, pc []byte
		if err := rows.Scan(&e.ID, &e.ChatID, &e.Text, &att, &e.CreatedAt, &e.Kind, &e.Note, &refs, &pc); err != nil {
			return nil, err
		}
		if len(pc) > 0 {
			var c PageContext
			if json.Unmarshal(pc, &c) == nil && c.Page != "" {
				e.Context = &c
			}
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
	return s.EnqueueUser(ctx, chatID, text, attachments, nil)
}

// EnqueueUser enqueues a message from the user with its page context (nil: none; checked by the
// caller).
func (s *Store) EnqueueUser(ctx context.Context, chatID, text string, attachments []string, pc *PageContext) (QueueEntry, error) {
	return s.enqueue(ctx, chatID, QueueUser, text, attachments, "", nil, pc)
}

// EnqueueKind enqueues an entry of the given kind (QueueUser, QueueSystem).
func (s *Store) EnqueueKind(ctx context.Context, chatID, kind, text string, attachments []string) (QueueEntry, error) {
	return s.enqueue(ctx, chatID, kind, text, attachments, "", nil, nil)
}

// EnqueueSystem enqueues an orchestrator note (note: NoteBackground, NoteSandbox).
func (s *Store) EnqueueSystem(ctx context.Context, chatID, note string, refs []string, text string) (QueueEntry, error) {
	return s.enqueue(ctx, chatID, QueueSystem, text, nil, note, refs, nil)
}

func (s *Store) enqueue(ctx context.Context, chatID, kind, text string, attachments []string, note string, refs []string, pc *PageContext) (QueueEntry, error) {
	if attachments == nil {
		attachments = []string{}
	}
	if refs == nil {
		refs = []string{}
	}
	if kind != QueueSystem {
		kind, note, refs = QueueUser, "", []string{}
	} else {
		pc = nil // only messages from the user carry a page context
	}
	att, _ := json.Marshal(attachments)
	rj, _ := json.Marshal(refs)
	var pj []byte // NULL without context
	if pc != nil {
		pj, _ = json.Marshal(pc)
	}
	rows, err := s.pool.Query(ctx, `INSERT INTO chat_queue (chat_id, text, attachments, kind, note, refs, context) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+queueCols,
		chatID, text, att, kind, note, rj, pj)
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

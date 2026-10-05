package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Durchgänge (Review 3, H1/H2): Jeder Auftrag an pi ist ein Durchgang mit Auslöser und Herkunft.
// Für die Evaluation belegt das, was der Nutzer beauftragt hat und was der Orchestrator von sich aus
// übergab; der Manager zählt daran die Weckrufe je Stunde und die Durchgänge ohne Nutzer in Folge.

// Auslöser eines Durchgangs.
const (
	TriggerUser  = "user"  // der Nutzer hat gesendet (Nachricht, „Jetzt senden“)
	TriggerQueue = "queue" // beim Laufende übergeben, mindestens eine Nachricht des Nutzers darunter
	TriggerWake  = "wake"  // nur Meldungen des Orchestrators, ohne Zutun des Nutzers (Weckruf)
)

// Herkunft eines Auftrags an pi.
const (
	OriginUser   = "user"   // nur Text des Nutzers
	OriginSystem = "system" // nur Meldungen des Orchestrators
	OriginMixed  = "mixed"  // beides
)

// Source ist ein Teil eines Auftrags, in Reihenfolge.
type Source struct {
	Kind    string   `json:"kind"`               // QueueUser oder QueueSystem
	Type    string   `json:"type,omitempty"`     // bei system: NoteBackground, NoteSandbox, NoteLanguage
	Refs    []string `json:"refs,omitempty"`     // bei system: betroffene Aufgaben
	QueueID string   `json:"queue_id,omitempty"` // Eintrag der Warteschlange, falls eingereiht
	// Marker: Marke des Zauns um die Daten aus der Sandbox (nur system, wenn es Daten gibt).
	Marker string `json:"marker,omitempty"`
}

// Turn ist ein Durchgang.
type Turn struct {
	ID        int64     `json:"id"`
	ChatID    string    `json:"chat_id"`
	Trigger   string    `json:"trigger"`
	Origin    string    `json:"origin"`
	Sources   []Source  `json:"sources"`
	QueueIDs  []string  `json:"queue_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateTurn legt einen Durchgang an (vor dem Auftrag an pi; scheitert er, DeleteTurn).
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

// DeleteTurn nimmt einen Durchgang zurück, dessen Auftrag pi nicht angenommen hat.
func (s *Store) DeleteTurn(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM chat_turns WHERE id=$1`, id)
	return err
}

// FirstTurnLanguage liefert die bevorzugte Sprache des Chats, solange er noch keinen Durchgang hat;
// sonst (und ohne Angabe) "".
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

// Turns liefert die Durchgänge eines Chats in Reihenfolge.
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

// WakesSince zählt die Weckrufe (Durchgänge ohne Zutun des Nutzers) eines Chats seit since.
func (s *Store) WakesSince(ctx context.Context, chatID string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM chat_turns WHERE chat_id=$1 AND trigger=$2 AND created_at >= $3`, chatID, TriggerWake, since).Scan(&n)
	return n, err
}

// AutoTurnsInRow zählt die Weckrufe seit dem letzten Durchgang, an dem der Nutzer beteiligt war.
func (s *Store) AutoTurnsInRow(ctx context.Context, chatID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM chat_turns
WHERE chat_id=$1 AND id > COALESCE((SELECT max(id) FROM chat_turns WHERE chat_id=$1 AND trigger<>$2), 0)`, chatID, TriggerWake).Scan(&n)
	return n, err
}

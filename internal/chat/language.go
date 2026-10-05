package chat

// Sprache des Nutzers. Der Agent antwortet in der Sprache der letzten Nachricht des Nutzers (Regel im
// Systemhinweis, internal/worker). Ist sie dort nicht erkennbar („ok“, ein Dateiname, nur Code), gilt
// die bevorzugte Sprache laut Browser: Die Oberfläche gibt sie beim Anlegen des Chats mit (language,
// BCP 47), und der Orchestrator sagt sie dem Agenten einmal, mit dem ersten Auftrag des Chats, als
// Meldung in der üblichen Hülle (origin.go). In den Systemhinweis gehört sie nicht: Plätze starten,
// bevor der Chat feststeht. Beim Fortsetzen bleibt die Sitzung von pi erhalten, die Meldung also im
// Kontext; sie wird nicht wiederholt.

import (
	"context"
	"fmt"
	"strings"

	"agw/internal/store"
)

// maxLanguageLen: längste zulässige Sprachangabe (BCP 47 sieht 35 Zeichen als Mindestpuffer vor).
const maxLanguageLen = 35

// NormalizeLanguage prüft eine Sprachangabe (BCP 47, etwa „en-US“): nur ASCII-Buchstaben, Ziffern und
// Bindestriche, höchstens 35 Zeichen, Teile durch genau einen Bindestrich getrennt, erster Teil aus
// Buchstaben. Leer (auch nur Leerraum) heißt „keine Angabe“.
func NormalizeLanguage(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > maxLanguageLen {
		return "", fmt.Errorf("%w: language darf höchstens %d Zeichen lang sein", ErrInvalid, maxLanguageLen)
	}
	for i, part := range strings.Split(s, "-") {
		if part == "" {
			return "", fmt.Errorf("%w: language ist keine Sprachangabe nach BCP 47", ErrInvalid)
		}
		for _, r := range part {
			letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			digit := r >= '0' && r <= '9'
			if !letter && !(digit && i > 0) {
				return "", fmt.Errorf("%w: language ist keine Sprachangabe nach BCP 47", ErrInvalid)
			}
		}
	}
	return s, nil
}

// languageNote ist die Meldung mit der bevorzugten Sprache (eine Zeile, keine Daten aus der Sandbox,
// also ohne Zaun). Die Angabe ist geprüft (NormalizeLanguage) und kann die Zeile nicht verlassen.
func languageNote(lang string) systemNote {
	return systemNote{Type: store.NoteLanguage, Refs: []string{lang},
		Summary: "Bevorzugte Sprache des Nutzers laut Browser: " + lang +
			". Antworte in der Sprache, in der der Nutzer schreibt; diese Angabe gilt nur, wenn das nicht erkennbar ist."}
}

// firstTurnLanguage: die Meldung zur Sprache, wenn der Chat eine Angabe hat und noch keinen Durchgang.
func (m *Manager) firstTurnLanguage(ctx context.Context, chatID string) *systemNote {
	lang, err := m.st.FirstTurnLanguage(ctx, chatID)
	if err != nil || lang == "" {
		return nil
	}
	n := languageNote(lang)
	return &n
}

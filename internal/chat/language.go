package chat

// User language. The agent answers in the language of the user's last message (rule in the system
// note, internal/worker). If it cannot be told from that message ("ok", a file name, only code), the
// preferred language according to the browser applies: the UI passes it along when the chat is
// created (language, BCP 47), and the orchestrator tells the agent once, with the chat's first
// message, as a note in the usual envelope (origin.go). It does not belong in the system note: slots
// start before the chat is known. On resuming, pi's session is kept, so the note stays in the
// context; it is not repeated.

import (
	"context"
	"fmt"
	"strings"

	"agw/internal/store"
)

// maxLanguageLen: longest permitted language tag (BCP 47 recommends 35 characters as the minimum buffer).
const maxLanguageLen = 35

// NormalizeLanguage checks a language tag (BCP 47, e.g. "en-US"): only ASCII letters, digits and
// hyphens, at most 35 characters, parts separated by exactly one hyphen, first part made of letters.
// Empty (including whitespace only) means "not given".
func NormalizeLanguage(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > maxLanguageLen {
		return "", fmt.Errorf("%w: language must be at most %d characters long", ErrInvalid, maxLanguageLen)
	}
	for i, part := range strings.Split(s, "-") {
		if part == "" {
			return "", fmt.Errorf("%w: language is not a BCP 47 language tag", ErrInvalid)
		}
		for _, r := range part {
			letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			digit := r >= '0' && r <= '9'
			if !letter && !(digit && i > 0) {
				return "", fmt.Errorf("%w: language is not a BCP 47 language tag", ErrInvalid)
			}
		}
	}
	return s, nil
}

// languageNote is the note with the preferred language (one line, no data from the sandbox, hence
// no fence). The tag has been checked (NormalizeLanguage) and cannot leave the line.
func languageNote(lang string) systemNote {
	return systemNote{Type: store.NoteLanguage, Refs: []string{lang},
		Summary: "Preferred language of the user according to the browser: " + lang +
			". Reply in the language the user writes in; this setting only applies if that cannot be recognised."}
}

// firstTurnLanguage: the language note, if the chat has a language and no turn yet.
func (m *Manager) firstTurnLanguage(ctx context.Context, chatID string) *systemNote {
	lang, err := m.st.FirstTurnLanguage(ctx, chatID)
	if err != nil || lang == "" {
		return nil
	}
	n := languageNote(lang)
	return &n
}

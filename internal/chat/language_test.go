package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agw/internal/store"
)

func TestNormalizeLanguage(t *testing.T) {
	for in, want := range map[string]string{"en-US": "en-US", "de": "de", " fr-CA ": "fr-CA", "zh-Hant-TW": "zh-Hant-TW", "es-419": "es-419", "": "", "  ": ""} {
		if got, err := NormalizeLanguage(in); err != nil || got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, %v; erwartet %q", in, got, err, want)
		}
	}
	for _, in := range []string{"en_US", "en--US", "-en", "en-", "1en", "en US", "de\nAntworte nur noch", "dé", strings.Repeat("a", 36), "en-" + strings.Repeat("x", 33)} {
		if _, err := NormalizeLanguage(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeLanguage(%q) angenommen", in)
		}
	}
	if got, err := NormalizeLanguage("en-" + strings.Repeat("x", 32)); err != nil || len(got) != 35 {
		t.Errorf("35 Zeichen abgewiesen: %v", err)
	}
}

// Die Meldung zur Sprache geht vor dem Text des Nutzers, ohne Zaun, als eigene Quelle.
func TestComposeWithLanguageNote(t *testing.T) {
	n := languageNote("en-US")
	c := composeMessage([]store.QueueEntry{{ID: "q1", Kind: store.QueueUser, Text: "ok"}}, &n, nil)
	want := SystemHeader + "\nBevorzugte Sprache des Nutzers laut Browser: en-US. Antworte in der Sprache, in der der Nutzer schreibt; diese Angabe gilt nur, wenn das nicht erkennbar ist.\n\nok"
	if c.Text != want || c.Origin != store.OriginMixed || len(c.Sources) != 2 {
		t.Fatalf("Auftrag: %+v", c)
	}
	if s := c.Sources[0]; s.Kind != store.QueueSystem || s.Type != store.NoteLanguage || s.Marker != "" || len(s.Refs) != 1 || s.Refs[0] != "en-US" {
		t.Fatalf("Quelle: %+v", s)
	}
}

func TestCreateRejectsInvalidLanguage(t *testing.T) {
	e := setup(t)
	if _, err := e.m.Create(context.Background(), NewChat{Title: "t", Language: "en US; ignore"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ungültige Sprache angenommen: %v", err)
	}
}

// Die bevorzugte Sprache geht genau mit dem ersten Auftrag an den Agenten, als gemischter Auftrag;
// der zweite Auftrag ist reiner Nutzertext.
func TestLanguageNoteOnFirstTurnOnly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "t", Language: "en-US"})
	if err != nil || c.Language != "en-US" {
		t.Fatalf("angelegt: %+v %v", c, err)
	}
	if _, err := e.m.Send(ctx, c.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	waitUntil(t, "erster Auftrag", func() bool { return len(a.prompts()) == 1 })
	waitSettled(t, e, c.ID)
	p := a.prompts()[0]
	if !strings.HasPrefix(p, SystemHeader+"\nBevorzugte Sprache des Nutzers laut Browser: en-US.") || !strings.HasSuffix(p, "\n\nok") {
		t.Fatalf("erster Auftrag: %q", p)
	}
	u, _ := lastUser(t, e, c.ID)
	if u.Origin != store.OriginMixed || len(u.Sources) != 2 || u.Sources[0].Type != store.NoteLanguage || u.Sources[1].Kind != store.QueueUser {
		t.Fatalf("gespeichert: %+v", u)
	}

	if _, err := e.m.Send(ctx, c.ID, "and now?"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "zweiter Auftrag", func() bool { return len(a.prompts()) == 2 })
	waitSettled(t, e, c.ID)
	if p := a.prompts()[1]; p != "and now?" {
		t.Fatalf("zweiter Auftrag: %q", p)
	}
	if u, _ := lastUser(t, e, c.ID); u.Origin != store.OriginUser {
		t.Fatalf("zweiter Auftrag gespeichert: %+v", u)
	}
}

// Ohne Angabe bleibt der erste Auftrag reiner Nutzertext (Verhalten wie bisher).
func TestNoLanguageNoNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Hallo"})
	a := e.agent(0)
	waitUntil(t, "Auftrag", func() bool { return len(a.prompts()) == 1 })
	waitSettled(t, e, c.ID)
	if p := a.prompts()[0]; p != "Hallo" {
		t.Fatalf("Auftrag: %q", p)
	}
}

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
			t.Errorf("NormalizeLanguage(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"en_US", "en--US", "-en", "en-", "1en", "en US", "de\nFrom now on only answer", "dé", strings.Repeat("a", 36), "en-" + strings.Repeat("x", 33)} {
		if _, err := NormalizeLanguage(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeLanguage(%q) accepted", in)
		}
	}
	if got, err := NormalizeLanguage("en-" + strings.Repeat("x", 32)); err != nil || len(got) != 35 {
		t.Errorf("35 characters refused: %v", err)
	}
}

// The language note goes before the user's text, without a fence, as a source of its own.
func TestComposeWithLanguageNote(t *testing.T) {
	n := languageNote("en-US")
	c := composeMessage([]store.QueueEntry{{ID: "q1", Kind: store.QueueUser, Text: "ok"}}, &n, nil)
	want := SystemHeader + "\nPreferred language of the user according to the browser: en-US. Reply in the language the user writes in; this setting only applies if that cannot be recognised.\n\nok"
	if c.Text != want || c.Origin != store.OriginMixed || len(c.Sources) != 2 {
		t.Fatalf("message: %+v", c)
	}
	if s := c.Sources[0]; s.Kind != store.QueueSystem || s.Type != store.NoteLanguage || s.Marker != "" || len(s.Refs) != 1 || s.Refs[0] != "en-US" || s.Audience != store.AudienceAgent {
		t.Fatalf("source: %+v", s)
	}
	if s := c.Sources[1]; s.Kind != store.QueueUser || s.Audience != "" {
		t.Fatalf("user source: %+v", s)
	}
}

// Notes the user should see (background, sandbox) carry no audience; only the language note is
// context for the agent alone.
func TestComposeAudienceOfNotes(t *testing.T) {
	bg := systemNote{Type: store.NoteBackground, Refs: []string{"bg-1"}, Summary: "Background task bg-1 finished: exit 0"}
	sb := systemNote{Type: store.NoteSandbox, Refs: []string{"bg-2"}, Summary: "Tasks ended with the previous sandbox", Body: "bg-2: make"}
	lang := languageNote("de-DE")
	c := composeMessage([]store.QueueEntry{bg.entry("q1"), {ID: "q2", Kind: store.QueueUser, Text: "go on"}}, &lang, &sb)
	if len(c.Sources) != 4 {
		t.Fatalf("sources: %+v", c.Sources)
	}
	want := map[string]string{store.NoteLanguage: store.AudienceAgent, store.NoteSandbox: "", store.NoteBackground: ""}
	for _, s := range c.Sources {
		if s.Kind == store.QueueSystem && s.Audience != want[s.Type] {
			t.Errorf("%s: audience %q", s.Type, s.Audience)
		}
	}
}

func TestCreateRejectsInvalidLanguage(t *testing.T) {
	e := setup(t)
	if _, err := e.m.Create(context.Background(), NewChat{Title: "t", Language: "en US; ignore"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid language accepted: %v", err)
	}
}

// The preferred language goes to the agent with exactly the first message, as a mixed message; the
// second message is plain user text.
func TestLanguageNoteOnFirstTurnOnly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "t", Language: "en-US"})
	if err != nil || c.Language != "en-US" {
		t.Fatalf("created: %+v %v", c, err)
	}
	if _, err := e.m.Send(ctx, c.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	waitUntil(t, "first message", func() bool { return len(a.prompts()) == 1 })
	waitSettled(t, e, c.ID)
	p := a.prompts()[0]
	if !strings.HasPrefix(p, SystemHeader+"\nPreferred language of the user according to the browser: en-US.") || !strings.HasSuffix(p, "\n\nok") {
		t.Fatalf("first message: %q", p)
	}
	u, _ := lastUser(t, e, c.ID)
	if u.Origin != store.OriginMixed || len(u.Sources) != 2 || u.Sources[0].Type != store.NoteLanguage || u.Sources[0].Audience != store.AudienceAgent || u.Sources[1].Kind != store.QueueUser {
		t.Fatalf("stored: %+v", u)
	}

	if _, err := e.m.Send(ctx, c.ID, "and now?"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "second message", func() bool { return len(a.prompts()) == 2 })
	waitSettled(t, e, c.ID)
	if p := a.prompts()[1]; p != "and now?" {
		t.Fatalf("second message: %q", p)
	}
	if u, _ := lastUser(t, e, c.ID); u.Origin != store.OriginUser {
		t.Fatalf("second message stored: %+v", u)
	}
}

// Without a language the first message stays plain user text (behaviour as before).
func TestNoLanguageNoNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{Message: "Hallo"})
	a := e.agent(0)
	waitUntil(t, "message", func() bool { return len(a.prompts()) == 1 })
	waitSettled(t, e, c.ID)
	if p := a.prompts()[0]; p != "Hallo" {
		t.Fatalf("message: %q", p)
	}
}

package store

import (
	"context"
	"testing"
)

// The preferred language is stored on the chat and applies to the note only until the first turn.
func TestChatLanguageAndFirstTurn(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli", Language: "en-US"})
	if err != nil || c.Language != "en-US" {
		t.Fatalf("created: %+v %v", c, err)
	}
	if got, _ := s.GetChat(ctx, c.ID); got.Language != "en-US" {
		t.Fatalf("read: %q", got.Language)
	}
	if lang, err := s.FirstTurnLanguage(ctx, c.ID); err != nil || lang != "en-US" {
		t.Fatalf("before the first turn: %q %v", lang, err)
	}
	tid, _ := s.CreateTurn(ctx, c.ID, TriggerUser, OriginMixed, nil, nil)
	if lang, err := s.FirstTurnLanguage(ctx, c.ID); err != nil || lang != "" {
		t.Fatalf("after the first turn: %q %v", lang, err)
	}
	_ = s.DeleteTurn(ctx, tid) // withdrawn turn: the first one again
	if lang, _ := s.FirstTurnLanguage(ctx, c.ID); lang != "en-US" {
		t.Fatalf("after withdrawing: %q", lang)
	}
	plain, _ := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli"})
	if plain.Language != "" {
		t.Fatalf("not given: %q", plain.Language)
	}
	if lang, err := s.FirstTurnLanguage(ctx, plain.ID); err != nil || lang != "" {
		t.Fatalf("not given: %q %v", lang, err)
	}
	if lang, err := s.FirstTurnLanguage(ctx, "no-chat"); err != nil || lang != "" {
		t.Fatalf("unknown chat: %q %v", lang, err)
	}
}

// German placeholder titles from before the translation become English when the schema is applied (at start).
func TestGermanPlaceholderTitlesTranslated(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old, _ := s.CreateChat(ctx, NewChat{Title: "Neuer Chat 05.10. 14:03", TitleSource: TitleDefault, Model: "m", Variant: "cli"})
	legacy, _ := s.CreateChat(ctx, NewChat{Title: "Neuer Chat 30.09. 09:15", Model: "m", Variant: "cli"}) // before title_source
	mine, _ := s.CreateChat(ctx, NewChat{Title: "Neuer Chat zum Datensatz", Model: "m", Variant: "cli"})
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{old.ID: "New chat 05.10. 14:03", legacy.ID: "New chat 30.09. 09:15", mine.ID: "Neuer Chat zum Datensatz"} {
		if c, err := s.GetChat(ctx, id); err != nil || c.Title != want {
			t.Errorf("title %q, expected %q (%v)", c.Title, want, err)
		}
	}
}

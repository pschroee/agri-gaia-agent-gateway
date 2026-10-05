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

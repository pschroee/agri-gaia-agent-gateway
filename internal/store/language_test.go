package store

import (
	"context"
	"testing"
)

// Die bevorzugte Sprache wird am Chat gespeichert und gilt für die Meldung nur bis zum ersten Durchgang.
func TestChatLanguageAndFirstTurn(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli", Language: "en-US"})
	if err != nil || c.Language != "en-US" {
		t.Fatalf("angelegt: %+v %v", c, err)
	}
	if got, _ := s.GetChat(ctx, c.ID); got.Language != "en-US" {
		t.Fatalf("gelesen: %q", got.Language)
	}
	if lang, err := s.FirstTurnLanguage(ctx, c.ID); err != nil || lang != "en-US" {
		t.Fatalf("vor dem ersten Durchgang: %q %v", lang, err)
	}
	tid, _ := s.CreateTurn(ctx, c.ID, TriggerUser, OriginMixed, nil, nil)
	if lang, err := s.FirstTurnLanguage(ctx, c.ID); err != nil || lang != "" {
		t.Fatalf("nach dem ersten Durchgang: %q %v", lang, err)
	}
	_ = s.DeleteTurn(ctx, tid) // zurückgenommener Durchgang: wieder der erste
	if lang, _ := s.FirstTurnLanguage(ctx, c.ID); lang != "en-US" {
		t.Fatalf("nach dem Zurücknehmen: %q", lang)
	}
	plain, _ := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli"})
	if plain.Language != "" {
		t.Fatalf("ohne Angabe: %q", plain.Language)
	}
	if lang, err := s.FirstTurnLanguage(ctx, plain.ID); err != nil || lang != "" {
		t.Fatalf("ohne Angabe: %q %v", lang, err)
	}
	if lang, err := s.FirstTurnLanguage(ctx, "kein-chat"); err != nil || lang != "" {
		t.Fatalf("unbekannter Chat: %q %v", lang, err)
	}
}

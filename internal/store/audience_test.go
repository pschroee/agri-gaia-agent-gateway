// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNoteAudience(t *testing.T) {
	for typ, want := range map[string]string{NoteLanguage: AudienceAgent, NoteContext: AudienceAgent, NoteBackground: "", NoteSandbox: "", "pi": "", "": ""} {
		if got := NoteAudience(typ); got != want {
			t.Errorf("NoteAudience(%q) = %q, want %q", typ, got, want)
		}
	}
}

// Rows stored before the field existed carry only the type; reading fills in the audience, on
// turns and on messages alike. User parts never get one.
func TestAudienceFilledOnRead(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli"})
	old := []Source{{Kind: QueueSystem, Type: NoteLanguage, Refs: []string{"de-DE"}}, {Kind: QueueUser}}
	turn, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginMixed, old, nil)
	if err != nil {
		t.Fatal(err)
	}
	user := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"x"}]}`)
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, &MessageMeta{TurnID: turn, Trigger: TriggerUser, Origin: OriginMixed, Sources: old}); err != nil {
		t.Fatal(err)
	}
	turns, _ := s.Turns(ctx, c.ID)
	msgs, _ := s.Messages(ctx, c.ID)
	if len(turns) != 1 || len(msgs) != 1 {
		t.Fatalf("turns %d, messages %d", len(turns), len(msgs))
	}
	for name, src := range map[string][]Source{"turn": turns[0].Sources, "message": msgs[0].Sources} {
		if len(src) != 2 || src[0].Audience != AudienceAgent || src[1].Audience != "" {
			t.Fatalf("%s: %+v", name, src)
		}
	}
	b, _ := json.Marshal(msgs[0])
	if !strings.Contains(string(b), `"audience":"agent"`) || strings.Count(string(b), `"audience"`) != 1 {
		t.Fatalf("JSON: %s", b)
	}
}

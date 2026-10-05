package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Review 3, H1/H2: turns with trigger and origin; messages carry them along.
func TestTurnsAndMessageOrigin(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli"})
	sys, err := s.EnqueueSystem(ctx, c.ID, NoteBackground, []string{"bg-1"}, "Background task bg-1 ended: exit 0\nCommand: make")
	if err != nil || sys.Kind != QueueSystem || sys.Note != NoteBackground || len(sys.Refs) != 1 || sys.Refs[0] != "bg-1" {
		t.Fatalf("system entry: %+v %v", sys, err)
	}
	if list, _ := s.ListQueue(ctx, c.ID); len(list) != 1 || list[0].Refs[0] != "bg-1" {
		t.Fatalf("list: %+v", list)
	}
	claimed, _ := s.ClaimQueue(ctx, c.ID)

	u1, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, []Source{{Kind: QueueUser}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w1, _ := s.CreateTurn(ctx, c.ID, TriggerWake, OriginSystem, []Source{{Kind: QueueSystem, Type: NoteBackground, Refs: []string{"bg-1"}, QueueID: sys.ID, Marker: "agw-0123456789abcdef"}}, []string{claimed[0].ID})
	w2, _ := s.CreateTurn(ctx, c.ID, TriggerWake, OriginSystem, nil, nil)
	if n, _ := s.WakesSince(ctx, c.ID, time.Now().Add(-time.Hour)); n != 2 {
		t.Fatalf("wake-ups: %d", n)
	}
	if n, _ := s.AutoTurnsInRow(ctx, c.ID); n != 2 {
		t.Fatalf("in a row without the user: %d", n)
	}
	if err := s.DeleteTurn(ctx, w2); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AutoTurnsInRow(ctx, c.ID); n != 1 {
		t.Fatalf("after deleting: %d", n)
	}
	q, _ := s.CreateTurn(ctx, c.ID, TriggerQueue, OriginMixed, nil, nil)
	if n, _ := s.AutoTurnsInRow(ctx, c.ID); n != 0 {
		t.Fatalf("after a turn with the user: %d", n)
	}
	turns, err := s.Turns(ctx, c.ID)
	if err != nil || len(turns) != 3 || turns[0].ID != u1 || turns[1].ID != w1 || turns[2].ID != q || turns[1].Sources[0].Marker != "agw-0123456789abcdef" || turns[1].QueueIDs[0] != sys.ID {
		t.Fatalf("turns: %+v %v", turns, err)
	}

	user := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"x"}]}`)
	answer := json.RawMessage(`{"role":"assistant","content":[]}`)
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, &MessageMeta{TurnID: w1, Trigger: TriggerWake, Origin: OriginSystem, Sources: turns[1].Sources}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurnMessage(ctx, c.ID, answer, nil, &MessageMeta{TurnID: w1, Trigger: TriggerWake}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBilledMessage(ctx, c.ID, answer, nil); err != nil { // without details (old rows)
		t.Fatal(err)
	}
	msgs, _ := s.Messages(ctx, c.ID)
	m0, m1, m2 := msgs[0], msgs[1], msgs[2]
	if m0.Origin != OriginSystem || m0.Trigger != TriggerWake || m0.TurnID == nil || *m0.TurnID != w1 || len(m0.Sources) != 1 || m0.Sources[0].Refs[0] != "bg-1" {
		t.Fatalf("user message: %+v", m0)
	}
	if m1.Origin != "" || m1.Trigger != TriggerWake || m1.Sources != nil {
		t.Fatalf("reply: %+v", m1)
	}
	if m2.Origin != "" || m2.Trigger != "" || m2.TurnID != nil {
		t.Fatalf("without details: %+v", m2)
	}
	b, _ := json.Marshal(m1)
	if string(b) == "" || json.Valid(b) == false {
		t.Fatal("JSON")
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["origin"]; ok {
		t.Fatalf("origin on the reply: %s", b)
	}
}

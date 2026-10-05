package store

import (
	"context"
	"errors"
	"testing"
)

func TestQueueLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "q", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Enqueue(ctx, c.ID, "eins", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Enqueue(ctx, c.ID, "zwei", []string{"daten.csv"})
	x, _ := s.Enqueue(ctx, c.ID, "drei", nil)
	if a.ID == "" || len(a.Attachments) != 0 || b.Attachments[0] != "daten.csv" {
		t.Fatalf("Einträge: %+v %+v", a, b)
	}
	if got, _ := s.GetChat(ctx, c.ID); got.Queued != 3 {
		t.Fatalf("Queued = %d", got.Queued)
	}
	if err := s.RemoveQueued(ctx, c.ID, x.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveQueued(ctx, c.ID, x.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zweites Entfernen: %v", err)
	}
	list, _ := s.ListQueue(ctx, c.ID)
	if len(list) != 2 || list[0].Text != "eins" || list[1].Text != "zwei" {
		t.Fatalf("Liste: %+v", list)
	}
	claimed, err := s.ClaimQueue(ctx, c.ID)
	if err != nil || len(claimed) != 2 || claimed[0].ID != a.ID || claimed[1].ID != b.ID {
		t.Fatalf("Übergabe: %+v %v", claimed, err)
	}
	if list, _ := s.ListQueue(ctx, c.ID); len(list) != 0 {
		t.Fatalf("nach Übergabe offen: %+v", list)
	}
	if err := s.RemoveQueued(ctx, c.ID, a.ID); !errors.Is(err, ErrDelivered) {
		t.Fatalf("Entfernen nach Übergabe: %v", err)
	}
	if again, _ := s.ClaimQueue(ctx, c.ID); len(again) != 0 {
		t.Fatalf("doppelt übergeben: %+v", again)
	}
	// Zurücknehmen (pi hat nicht angenommen): wieder offen, Reihenfolge bleibt.
	if err := s.UnclaimQueue(ctx, []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListQueue(ctx, c.ID); len(list) != 2 || list[0].ID != a.ID {
		t.Fatalf("nach Zurücknehmen: %+v", list)
	}
	if got, _ := s.GetChat(ctx, c.ID); got.Queued != 2 {
		t.Fatalf("Queued nach Zurücknehmen = %d", got.Queued)
	}
}

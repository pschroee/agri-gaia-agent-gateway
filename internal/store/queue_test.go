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
	a, err := s.Enqueue(ctx, c.ID, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Enqueue(ctx, c.ID, "two", []string{"data.csv"})
	x, _ := s.Enqueue(ctx, c.ID, "three", nil)
	if a.ID == "" || len(a.Attachments) != 0 || b.Attachments[0] != "data.csv" {
		t.Fatalf("entries: %+v %+v", a, b)
	}
	if got, _ := s.GetChat(ctx, c.ID); got.Queued != 3 {
		t.Fatalf("Queued = %d", got.Queued)
	}
	if err := s.RemoveQueued(ctx, c.ID, x.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveQueued(ctx, c.ID, x.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second removal: %v", err)
	}
	list, _ := s.ListQueue(ctx, c.ID)
	if len(list) != 2 || list[0].Text != "one" || list[1].Text != "two" {
		t.Fatalf("list: %+v", list)
	}
	claimed, err := s.ClaimQueue(ctx, c.ID)
	if err != nil || len(claimed) != 2 || claimed[0].ID != a.ID || claimed[1].ID != b.ID {
		t.Fatalf("delivery: %+v %v", claimed, err)
	}
	if list, _ := s.ListQueue(ctx, c.ID); len(list) != 0 {
		t.Fatalf("open after delivery: %+v", list)
	}
	if err := s.RemoveQueued(ctx, c.ID, a.ID); !errors.Is(err, ErrDelivered) {
		t.Fatalf("removal after delivery: %v", err)
	}
	if again, _ := s.ClaimQueue(ctx, c.ID); len(again) != 0 {
		t.Fatalf("delivered twice: %+v", again)
	}
	// Withdraw (pi did not accept): open again, order stays.
	if err := s.UnclaimQueue(ctx, []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListQueue(ctx, c.ID); len(list) != 2 || list[0].ID != a.ID {
		t.Fatalf("after withdrawing: %+v", list)
	}
	if got, _ := s.GetChat(ctx, c.ID); got.Queued != 2 {
		t.Fatalf("Queued after withdrawing = %d", got.Queued)
	}
}

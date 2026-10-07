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

// A user entry keeps its page context through the queue (list, delivery, reopening); system entries
// never carry one.
func TestQueuePageContext(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "q", Model: "m", Variant: "cli"})
	pc := &PageContext{Page: "datasets", Object: &ContextObject{Kind: "dataset", ID: "42", Name: "bay-3"}}
	a, err := s.EnqueueUser(ctx, c.ID, "one", nil, pc)
	if err != nil || a.Context == nil || a.Context.Object.Name != "bay-3" {
		t.Fatalf("enqueued: %+v %v", a, err)
	}
	b, _ := s.Enqueue(ctx, c.ID, "two", nil)
	n, _ := s.EnqueueSystem(ctx, c.ID, NoteBackground, []string{"bg-1"}, "done")
	if b.Context != nil || n.Context != nil {
		t.Fatalf("context without one: %+v %+v", b, n)
	}
	claimed, _ := s.ClaimQueue(ctx, c.ID)
	if len(claimed) != 3 || claimed[0].Context == nil || *claimed[0].Context.Object != *pc.Object || claimed[1].Context != nil {
		t.Fatalf("claimed: %+v", claimed)
	}
	if err := s.UnclaimQueue(ctx, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListQueue(ctx, c.ID)
	if len(list) != 1 || list[0].Context == nil || list[0].Context.Page != "datasets" {
		t.Fatalf("reopened: %+v", list)
	}
	// issue #45: a list of objects comes back in order; List reads old rows (object only) as well
	many := &PageContext{Page: "datasets", Objects: []ContextObject{{Kind: "dataset", ID: "42", Name: "bay-3"}, {Kind: "dataset", ID: "7"}}}
	m, _ := s.EnqueueUser(ctx, c.ID, "three", nil, many)
	list, _ = s.ListQueue(ctx, c.ID)
	if len(list) != 2 || list[1].ID != m.ID || list[1].Context == nil || len(list[1].Context.List()) != 2 || list[1].Context.List()[1].ID != "7" {
		t.Fatalf("list of objects: %+v", list)
	}
	if got := list[0].Context.List(); len(got) != 1 || got[0] != *pc.Object {
		t.Fatalf("single object: %+v", got)
	}
}

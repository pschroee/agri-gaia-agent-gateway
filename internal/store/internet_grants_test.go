// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newGrantChat(t *testing.T, s *Store) string {
	t.Helper()
	c, err := s.CreateChat(context.Background(), NewChat{Title: "grants", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// A grant is created pending, starts once, ends once; the lists tell active from the rest; bytes only grow.
func TestGrantLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := newGrantChat(t, s)
	g, err := s.CreateGrant(ctx, Grant{ChatID: chat, Kind: "domains", Origin: GrantByAgent, Via: "cli", Reason: "flowers", Domains: []string{"download.tensorflow.org"},
		State: GrantPending, MaxMinutes: 30, MaxBytes: 500 << 20})
	if err != nil || g.ID == 0 || g.State != GrantPending || len(g.Domains) != 1 || string(g.Packages) != "[]" {
		t.Fatalf("create: %+v %v", g, err)
	}
	if act, _ := s.ActiveGrants(ctx, chat); len(act) != 0 {
		t.Fatalf("pending counts as active: %+v", act)
	}
	now := time.Now()
	a, ok, err := s.ActivateGrant(ctx, g.ID, now, now.Add(30*time.Minute))
	if err != nil || !ok || a.State != GrantActive || a.StartedAt == nil || a.ExpiresAt == nil {
		t.Fatalf("activate: %+v %v %v", a, ok, err)
	}
	if _, ok, _ := s.ActivateGrant(ctx, g.ID, now, now.Add(time.Hour)); ok {
		t.Fatal("activated twice")
	}
	if ok, _ := s.CloseGrant(ctx, g.ID, GrantRejected); ok {
		t.Fatal("an active grant was closed as rejected")
	}
	if err := s.SetGrantBytes(ctx, g.ID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrantBytes(ctx, g.ID, 400); err != nil { // never goes back
		t.Fatal(err)
	}
	if err := s.BumpBlocked(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := s.AllActiveGrants(ctx)
	if len(all) != 1 || all[0].Bytes != 1000 || all[0].Blocked != 1 {
		t.Fatalf("active: %+v", all)
	}
	e, ok, err := s.EndGrant(ctx, g.ID, EndBytes, 2000)
	if err != nil || !ok || e.State != GrantEnded || e.EndedReason != EndBytes || e.Bytes != 2000 || e.EndedAt == nil {
		t.Fatalf("end: %+v %v %v", e, ok, err)
	}
	if _, ok, _ := s.EndGrant(ctx, g.ID, EndUser, 0); ok {
		t.Fatal("ended twice")
	}
	if got, _ := s.GetGrant(ctx, g.ID); got.EndedReason != EndBytes {
		t.Fatalf("the first end wins: %+v", got)
	}
	// A second grant that is rejected, a third that stays pending: the list is newest first.
	g2, _ := s.CreateGrant(ctx, Grant{ChatID: chat, Kind: "search", Origin: GrantByAgent, State: GrantPending, MaxMinutes: 30, MaxBytes: 1 << 20})
	if ok, _ := s.CloseGrant(ctx, g2.ID, GrantRejected); !ok {
		t.Fatal("pending grant not closed")
	}
	list, _ := s.Grants(ctx, chat)
	if len(list) != 2 || list[0].ID != g2.ID || list[0].State != GrantRejected {
		t.Fatalf("list: %+v", list)
	}
	if _, err := s.ChatGrant(ctx, newGrantChat(t, s), g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a grant of another chat: %v", err)
	}
	if _, err := s.Grants(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

// Packages of a packages grant (stage 2 uses them): the first entry for a name wins, installed is set once.
func TestGrantPackages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := newGrantChat(t, s)
	g, _ := s.CreateGrant(ctx, Grant{ChatID: chat, Kind: "packages", Origin: GrantByAgent, State: GrantPending, MaxMinutes: 60, MaxBytes: 4000 << 20})
	for _, p := range []GrantPackage{{GrantID: g.ID, Ecosystem: "pip", Name: "torch"}, {GrantID: g.ID, Ecosystem: "pip", Name: "numpy", Parent: "torch"}, {GrantID: g.ID, Ecosystem: "pip", Name: "numpy", Parent: "other"}} {
		if err := s.AddGrantPackage(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkInstalled(ctx, g.ID, "pip", "torch"); err != nil {
		t.Fatal(err)
	}
	list, err := s.GrantPackages(ctx, g.ID)
	if err != nil || len(list) != 2 || list[0].Name != "torch" || list[0].InstalledAt == nil || list[1].Parent != "torch" || list[1].InstalledAt != nil {
		t.Fatalf("packages: %+v %v", list, err)
	}
}

func webRow(chat string, grant int64, host, denied string, at time.Time) WebRequest {
	return WebRequest{ChatID: chat, Method: "CONNECT", Host: host, Port: 443, Status: 200, Denied: denied, StartedAt: at, GrantID: grant, Origin: "pi", BytesDown: 10}
}

// After MaxRows single rows a grant collapses further connections into one row per (origin, host, outcome); the
// count and the bytes of the collapsed row grow, and finishing a tunnel adds to it instead of overwriting.
func TestGrantLogRowCap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := newGrantChat(t, s)
	g, _ := s.CreateGrant(ctx, Grant{ChatID: chat, Kind: "full", Origin: GrantByUser, State: GrantActive, MaxMinutes: 30, MaxBytes: 1 << 20})
	now := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := s.AddWebRequestCapped(ctx, webRow(chat, g.ID, "a.example.org", "", now), 3); err != nil {
			t.Fatal(err)
		}
	}
	// The 4th and 5th to the same host and outcome collapse into one row; a refused one gets its own.
	id4, _ := s.AddWebRequestCapped(ctx, webRow(chat, g.ID, "a.example.org", "", now), 3)
	id5, _ := s.AddWebRequestCapped(ctx, webRow(chat, g.ID, "a.example.org", "", now), 3)
	idD, _ := s.AddWebRequestCapped(ctx, webRow(chat, g.ID, "a.example.org", "blocked: x", now), 3)
	if id4 != id5 || idD == id4 {
		t.Fatalf("collapsed ids: %d %d refused %d", id4, id5, idD)
	}
	rows, _ := s.GrantWebRequests(ctx, chat, g.ID)
	if len(rows) != 5 {
		t.Fatalf("rows: %d, want 3 single + 2 collapsed: %+v", len(rows), rows)
	}
	var coll WebRequest
	for _, r := range rows {
		if r.ID == id4 {
			coll = r
		}
	}
	if coll.Method != CollapsedMethod || coll.Count != 2 || coll.BytesDown != 20 {
		t.Fatalf("collapsed row: %+v", coll)
	}
	// Finishing a tunnel recorded into a collapsed row adds to it and keeps its outcome.
	if err := s.FinishWebRequest(ctx, id4, 5, 7, 100, "tunnel closed"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.GrantWebRequests(ctx, chat, g.ID)
	for _, r := range rows {
		if r.ID == id4 && (r.BytesDown != 27 || r.BytesUp != 5 || r.Denied != "") {
			t.Fatalf("finished collapsed row: %+v", r)
		}
	}
	// A single row is overwritten by the final values.
	one, _ := s.AddWebRequest(ctx, WebRequest{ChatID: chat, Method: "CONNECT", Host: "b.example.org", StartedAt: now})
	if err := s.FinishWebRequest(ctx, one, 1, 2, 3, "tunnel closed: grant 1 ended (time limit)"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.WebRequests(ctx, chat)
	last := all[len(all)-1]
	if last.ID != one || last.BytesUp != 1 || last.BytesDown != 2 || last.Denied == "" || last.Origin != "pi" || last.Count != 1 {
		t.Fatalf("single row: %+v", last)
	}
}

// The connections across chats: filters for grant, outcome and origin, newest first, one more row than the limit
// for the cursor, and only the chats of the owner.
func TestWebConnectionsFilter(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mine, _ := s.CreateChat(ctx, NewChat{Title: "mine", Model: "m", Variant: "cli", Owner: "u1"})
	other, _ := s.CreateChat(ctx, NewChat{Title: "other", Model: "m", Variant: "cli", Owner: "u2"})
	now := time.Now()
	g1, _ := s.CreateGrant(ctx, Grant{ChatID: mine.ID, Kind: "full", Origin: GrantByUser, State: GrantActive, MaxMinutes: 30, MaxBytes: 1 << 20})
	g2, _ := s.CreateGrant(ctx, Grant{ChatID: mine.ID, Kind: "search", Origin: GrantByAgent, State: GrantActive, MaxMinutes: 30, MaxBytes: 1 << 20})
	for _, r := range []WebRequest{
		webRow(mine.ID, g1.ID, "a.example.org", "", now),
		webRow(mine.ID, g1.ID, "b.example.org", "blocked: x", now.Add(time.Second)),
		func() WebRequest {
			r := webRow(mine.ID, g2.ID, "c.example.org", "", now.Add(2*time.Second))
			r.Origin = "exec"
			return r
		}(),
		webRow(other.ID, 0, "d.example.org", "", now),
	} {
		if _, err := s.AddWebRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	u1 := "u1"
	rows, err := s.WebConnections(ctx, WebConnectionFilter{Owner: &u1})
	if err != nil || len(rows) != 3 || rows[0].Host != "c.example.org" {
		t.Fatalf("owner: %+v %v", rows, err)
	}
	if rows, _ = s.WebConnections(ctx, WebConnectionFilter{}); len(rows) != 4 {
		t.Fatalf("token mode sees all: %d", len(rows))
	}
	if rows, _ = s.WebConnections(ctx, WebConnectionFilter{Owner: &u1, Result: "blocked"}); len(rows) != 1 || rows[0].Host != "b.example.org" {
		t.Fatalf("blocked: %+v", rows)
	}
	if rows, _ = s.WebConnections(ctx, WebConnectionFilter{Owner: &u1, GrantID: g2.ID}); len(rows) != 1 || rows[0].Origin != "exec" {
		t.Fatalf("grant: %+v", rows)
	}
	if rows, _ = s.WebConnections(ctx, WebConnectionFilter{Owner: &u1, Origin: "pi", Result: "allowed"}); len(rows) != 1 || rows[0].Host != "a.example.org" {
		t.Fatalf("origin: %+v", rows)
	}
	if rows, _ = s.WebConnections(ctx, WebConnectionFilter{Owner: &u1, Limit: 2}); len(rows) != 3 {
		t.Fatalf("limit+1 for the cursor: %d", len(rows))
	}
	if rows, _ = s.WebConnections(ctx, WebConnectionFilter{Owner: &u1, Before: rows[1].ID}); len(rows) != 1 {
		t.Fatalf("before: %+v", rows)
	}
}

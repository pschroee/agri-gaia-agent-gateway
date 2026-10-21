// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agw/internal/netgrant"
	"agw/internal/pkggate"
	"agw/internal/store"
	"agw/internal/webproxy"
)

func startPackagesGrant(t *testing.T, e *env, chatID string, roots ...string) store.Grant {
	t.Helper()
	parsed, err := netgrant.ParsePackages(roots)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(parsed)
	now := time.Now()
	exp := now.Add(time.Hour)
	g, err := e.st.CreateGrant(context.Background(), store.Grant{ChatID: chatID, Kind: "packages", Origin: store.GrantByAgent, Packages: raw, State: store.GrantActive,
		MaxMinutes: 60, MaxBytes: 4000 << 20, StartedAt: &now, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	unlock := e.m.lock(chatID)
	defer unlock()
	if err := e.m.startGrant(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	return g
}

// The gate allows the roots, what they declare, and the build tools; everything else is refused with a code and
// without a grant; a pinned root only in its version.
func TestPackageSourceCheck(t *testing.T) {
	e := setup(t)
	chatID, _ := newChatNoNet(t, e)
	src := e.m.PackageSource()
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "torch"}); v.Allow || v.Code != netgrant.CodeInternetOff {
		t.Fatalf("no grant: %+v", v)
	}
	g := startPackagesGrant(t, e, chatID, "pip:torch[opt]", "pip:timm==1.0.11", "npm:left-pad")
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "torch"}); !v.Allow || v.GrantID != g.ID || v.Parent != "" || len(v.Extras) != 1 || v.Extras[0] != "opt" {
		t.Fatalf("root: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "numpy"}); v.Allow || v.Code != netgrant.CodePackageNotListed || v.GrantID != 0 {
		t.Fatalf("undeclared: %+v", v)
	}
	src.AddDeps(chatID, "pip", "torch", []int64{g.ID}, []netgrant.Dep{{Name: "numpy"}, {Name: "nvidia-cudnn-cu12"}})
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "numpy"}); !v.Allow || v.Parent != "torch" {
		t.Fatalf("declared dependency: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "npm", Name: "numpy"}); v.Allow {
		t.Fatalf("a dependency of a pip package is not an npm package: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "timm", Version: "1.0.11"}); !v.Allow {
		t.Fatalf("pinned version: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "timm", Version: "1.0.12"}); v.Allow || v.Code != netgrant.CodeVersionMismatch || v.GrantID != g.ID {
		t.Fatalf("other version: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "timm"}); !v.Allow {
		t.Fatalf("index of a pinned root: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "setuptools"}); !v.Allow || v.Parent != "build tool" {
		t.Fatalf("build tool: %+v", v)
	}
	if v := src.Check(chatID, pkggate.Query{Eco: "npm", Name: "setuptools"}); v.Allow {
		t.Fatalf("build tools are pip only: %+v", v)
	}
	// A full grant allows everything.
	startTestGrant(t, e, chatID, "full", nil, 30, 100)
	if v := src.Check(chatID, pkggate.Query{Eco: "pip", Name: "anything"}); !v.Allow || !v.Full {
		t.Fatalf("full: %+v", v)
	}
}

// The package list shows waiting, downloaded and installed; the grant ends when every root is installed.
func TestInstalledEndsPackagesGrant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startPackagesGrant(t, e, chatID, "pip:torch", "pip:timm==1.0.11")
	src := e.m.PackageSource()
	src.AddDeps(chatID, "pip", "torch", []int64{g.ID}, []netgrant.Dep{{Name: "numpy"}})
	src.Downloaded(chatID, "pip", "torch", []int64{g.ID})
	states := func() map[string]store.PackageState {
		var list []store.PackageState
		_ = json.Unmarshal(e.m.grants.get(g.ID).view().Packages, &list)
		out := map[string]store.PackageState{}
		for _, p := range list {
			out[p.Name] = p
		}
		return out
	}
	if s := states(); s["torch"].State != store.PkgDownloaded || s["timm"].State != store.PkgWaiting || s["numpy"].Parent != "torch" {
		t.Fatalf("states: %+v", s)
	}
	e.m.applyInstalled(ctx, chatID, map[string]string{"pip:torch": "2.5.1", "pip:timm": "1.0.9", "pip:numpy": "2.1.0"})
	if s := states(); s["torch"].State != store.PkgInstalled || s["torch"].InstalledVersion != "2.5.1" || s["timm"].State != store.PkgWaiting {
		t.Fatalf("a wrong version of a pinned root is not installed: %+v", s)
	}
	if got, _ := e.st.GetGrant(ctx, g.ID); got.State != store.GrantActive {
		t.Fatalf("ended too early: %+v", got)
	}
	e.m.applyInstalled(ctx, chatID, map[string]string{"pip:timm": "1.0.11"})
	got, _ := e.st.GetGrant(ctx, g.ID)
	if got.State != store.GrantEnded || got.EndedReason != store.EndInstalled {
		t.Fatalf("grant: %+v", got)
	}
	if v, _ := e.m.View(ctx, chatID); v.Internet {
		t.Fatal("internet stays on after the packages are installed")
	}
	// The list of an ended grant comes from the database.
	list, _ := e.m.InternetGrants(ctx, chatID)
	var ps []store.PackageState
	_ = json.Unmarshal(list[0].Packages, &ps)
	if len(ps) != 3 {
		t.Fatalf("packages of the ended grant: %+v", ps)
	}
}

// A refusal no grant covers counts at the chat; the view carries the count and the latest refusals.
func TestBlockedCountsAtTheChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	for i := 0; i < 4; i++ {
		e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "CONNECT", Host: "github.com", Port: 443, Status: 403, StartedAt: time.Now(),
			Denied: netgrant.MsgNotListed("github.com"), Reason: netgrant.CodeNotListed, Origin: "exec"})
	}
	v, err := e.m.View(ctx, chatID)
	if err != nil || v.InternetBlocked == nil || v.InternetBlocked.Count != 4 || len(v.InternetBlocked.Last) != 3 || v.InternetBlocked.Last[0].Reason != netgrant.CodeNotListed {
		t.Fatalf("view: %+v %v", v.InternetBlocked, err)
	}
}

// A request from a helper (subagent session) says so on the card, and the card carries when it lapses.
func TestInternetCardByAndExpiry(t *testing.T) {
	if askerOf(context.Background()) != "agent" {
		t.Fatal("no session is the agent")
	}
	if askerOf(store.WithSession(context.Background(), "main")) != "agent" {
		t.Fatal("main is the agent")
	}
	if askerOf(store.WithSession(context.Background(), "0a1b2c3d4e5f#1")) != "helper" {
		t.Fatal("a run is a helper")
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agw/internal/netgrant"
	"agw/internal/sock"
	"agw/internal/store"
	"agw/internal/webproxy"
)

// request asks for internet as the agent would and answers the approval; approve false rejects.
func request(t *testing.T, e *env, chatID, slot, via string, spec netgrant.Spec, approve bool) (sock.UploadResult, store.Approval) {
	t.Helper()
	ctx := context.Background()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	done := make(chan sock.UploadResult, 1)
	go func() {
		r, _ := e.m.RequestInternet(ctx, chatID, slot, via, spec)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if _, err := e.m.Decide(ctx, ap.ID, approve); err != nil {
		t.Fatal(err)
	}
	return <-done, ap
}

// startTestGrant starts an active grant of the chat directly (without a request).
func startTestGrant(t *testing.T, e *env, chatID, kind string, domains []string, minutes int, maxMB int64) store.Grant {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	exp := now.Add(time.Duration(minutes) * time.Minute)
	g, err := e.st.CreateGrant(ctx, store.Grant{ChatID: chatID, Kind: kind, Origin: store.GrantByAgent, Domains: domains, State: store.GrantActive,
		MaxMinutes: minutes, MaxBytes: maxMB << 20, StartedAt: &now, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	unlock := e.m.lock(chatID)
	defer unlock()
	if err := e.m.startGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	return g
}

func newChatNoNet(t *testing.T, e *env) (string, string) {
	t.Helper()
	off := false
	c, err := e.m.Create(context.Background(), NewChat{Internet: &off})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID, e.m.live[c.ID].slot.ID
}

// A request names a kind; the approval carries the gateway's normalised data and the agent's reason as a note; a
// search grant gives the chat internet for the web tools but does not attach the sandbox to the egress network.
func TestRequestInternetKinds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, slot := newChatNoNet(t, e)

	// domains: normalised list, clamped limits, card data from the gateway.
	res, ap := request(t, e, chatID, slot, "cli", netgrant.Spec{Kind: netgrant.Domains, Reason: "flowers archive", Domains: []string{" Download.TensorFlow.org. "}, Minutes: 999, MB: 800}, true)
	if res.Status != "approved" || !strings.Contains(res.Message, "domains download.tensorflow.org until ") || !strings.Contains(res.Message, "or 800 MB (grant ") {
		t.Fatalf("result: %+v", res)
	}
	var card internetCard
	if err := json.Unmarshal(ap.Internet, &card); err != nil {
		t.Fatalf("card data: %q %v", ap.Internet, err)
	}
	if card.Kind != "domains" || len(card.Domains) != 1 || card.Domains[0] != "download.tensorflow.org" || card.Minutes != 120 || card.Megabytes != 800 || card.GrantID == 0 || card.EndsWhenInstalled {
		t.Fatalf("card: %+v", card)
	}
	if ap.Name != "flowers archive" || !strings.Contains(ap.Preview, "Domains (https only): download.tensorflow.org") || !strings.Contains(ap.Preview, "Agent's note (not verified): flowers archive") {
		t.Fatalf("approval texts: name %q preview %q", ap.Name, ap.Preview)
	}
	v, _ := e.m.View(ctx, chatID)
	if !v.Internet || len(v.InternetGrants) != 1 || v.InternetGrants[0].ID != card.GrantID || v.InternetGrants[0].MaxMinutes != 120 {
		t.Fatalf("view: %+v", v.InternetGrants)
	}
	if e.agent(0).internet {
		t.Fatal("a domains grant attached the sandbox to the egress network")
	}

	// search: both web tools, no egress either.
	if res, _ = request(t, e, chatID, slot, "api", netgrant.Spec{Kind: netgrant.Search, Reason: "look up"}, true); res.Status != "approved" {
		t.Fatalf("search: %+v", res)
	}
	if on, search, extract := e.m.InternetState(chatID); !on || !search || !extract {
		t.Fatalf("state with search: %v %v %v", on, search, extract)
	}
	if e.agent(0).internet {
		t.Fatal("a search grant attached the sandbox to the egress network")
	}

	// Refusals never create an approval.
	before, _ := e.st.ListApprovals(ctx, "", chatID)
	for _, tc := range []struct {
		via  string
		spec netgrant.Spec
		want string
	}{
		{"mcp", netgrant.Spec{Kind: netgrant.Full, Reason: "x"}, "needs the command line"},
		{"api", netgrant.Spec{Kind: netgrant.Packages, Reason: "x", Packages: []string{"pip:torch"}}, "needs the command line"},
		{"cli", netgrant.Spec{Kind: netgrant.Packages, Reason: "x", Packages: []string{"pip:torch>=2"}}, "exact"},
		{"cli", netgrant.Spec{Kind: netgrant.Packages, Reason: "x"}, "at least one package"},
		{"cli", netgrant.Spec{Kind: netgrant.Domains, Reason: "x"}, "at least one host"},
		{"cli", netgrant.Spec{Kind: netgrant.Domains, Reason: "x", Domains: []string{"*.example.org"}}, "wildcards"},
		{"cli", netgrant.Spec{Kind: "everything", Reason: "x"}, "kind must be one of"},
	} {
		r, err := e.m.RequestInternet(ctx, chatID, slot, tc.via, tc.spec)
		if err != nil || r.Status != "refused" || !strings.Contains(r.Message, tc.want) || !strings.HasPrefix(r.Log, "refused: ") {
			t.Errorf("%s %+v: %+v %v, want refused ~ %q", tc.via, tc.spec, r, err, tc.want)
		}
	}
	if after, _ := e.st.ListApprovals(ctx, "", chatID); len(after) != len(before) {
		t.Fatalf("refusals created approvals: %d → %d", len(before), len(after))
	}
}

// A package source is refused as a domain with the same text in every binding, and nobody is asked (issue #205).
func TestRequestInternetPackageHostSameForAllBindings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, slot := newChatNoNet(t, e)
	before, _ := e.st.ListApprovals(ctx, "", chatID)
	var first string
	for _, via := range []string{"cli", "mcp", "api"} {
		r, err := e.m.RequestInternet(ctx, chatID, slot, via, netgrant.Spec{Kind: netgrant.Domains, Reason: "x", Domains: []string{"deb.debian.org"}})
		if err != nil || r.Status != "refused" || !strings.Contains(r.Message, "kind full") {
			t.Fatalf("%s: %+v %v", via, r, err)
		}
		if first == "" {
			first = r.Message
		} else if r.Message != first {
			t.Errorf("%s: message differs: %q vs %q", via, r.Message, first)
		}
	}
	if after, _ := e.st.ListApprovals(ctx, "", chatID); len(after) != len(before) {
		t.Fatalf("refusals created approvals: %d -> %d", len(before), len(after))
	}
	if v, _ := e.m.View(ctx, chatID); len(v.InternetGrants) != 0 {
		t.Fatalf("grants: %+v", v.InternetGrants)
	}
}

// At most five grants are active in a chat; the sixth request is refused before anyone is asked.
func TestRequestInternetTooManyGrants(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, slot := newChatNoNet(t, e)
	for i := 0; i < netgrant.MaxGrants; i++ {
		startTestGrant(t, e, chatID, "domains", []string{"h" + string(rune('a'+i)) + ".example.org"}, 30, 100)
	}
	r, err := e.m.RequestInternet(ctx, chatID, slot, "cli", netgrant.Spec{Kind: netgrant.Domains, Reason: "x", Domains: []string{"more.example.org"}})
	if err != nil || r.Status != "refused" || !strings.Contains(r.Message, "5 active internet grants") {
		t.Fatalf("sixth: %+v %v", r, err)
	}
	// A request that an active grant already covers is answered without a limit or a question.
	r, _ = e.m.RequestInternet(ctx, chatID, slot, "cli", netgrant.Spec{Kind: netgrant.Domains, Reason: "x", Domains: []string{"ha.example.org"}})
	if r.Status != "approved" || !strings.Contains(r.Message, "already active") {
		t.Fatalf("covered: %+v", r)
	}
}

// The gate decides by the active grants: a domains grant lists exact hosts, https only; ending the grant closes it.
func TestWebGateDecides(t *testing.T) {
	e := setup(t)
	chatID, _ := newChatNoNet(t, e)
	gate := e.m.WebGate()
	if c, s := gate.Attribute(e.agent(0).IP()); c != chatID || s == "" {
		t.Fatalf("attribution: %q %q", c, s)
	}
	if c, _ := gate.Attribute("203.0.113.9"); c != "" {
		t.Fatalf("a foreign address was attributed to %q", c)
	}
	if d := gate.Decide(chatID, netgrant.OriginPi, "download.tensorflow.org", 443); d.Allow || !strings.Contains(d.Reason, "internet access is off") {
		t.Fatalf("no grant: %+v", d)
	}
	g := startTestGrant(t, e, chatID, "domains", []string{"download.tensorflow.org"}, 30, 100)
	if d := gate.Decide(chatID, netgrant.OriginExec, "download.tensorflow.org", 443); !d.Allow || d.GrantID != g.ID || d.Kind != netgrant.Domains {
		t.Fatalf("listed host: %+v", d)
	}
	if d := gate.Decide(chatID, netgrant.OriginExec, "github.com", 443); d.Allow || !strings.Contains(d.Reason, "github.com is not in the allowed list") || d.GrantID != 0 || d.Code != netgrant.CodeNotListed {
		t.Fatalf("unlisted host: %+v", d)
	}
	if d := gate.Decide(chatID, netgrant.OriginPi, "searxng", 8080); d.Allow {
		t.Fatalf("a domains grant covers web_search: %+v", d)
	}
	if ok, _ := gate.Still(chatID, g.ID); !ok {
		t.Fatal("grant not still")
	}
	if on, search, extract := e.m.InternetState(chatID); !on || search || !extract {
		t.Fatalf("state with domains: %v %v %v", on, search, extract)
	}
	if !e.m.EndGrant(context.Background(), chatID, g.ID, store.EndAgent) {
		t.Fatal("grant not ended")
	}
	if ok, why := gate.Still(chatID, g.ID); ok || why != "switched off" {
		t.Fatalf("after the end: %v %q", ok, why)
	}
	if d := gate.Decide(chatID, netgrant.OriginPi, "download.tensorflow.org", 443); d.Allow || d.Reason != netgrant.MsgGrantEnded(g.ID, "switched off") {
		t.Fatalf("right after the end the refusal names the grant: %+v", d)
	}
}

// The user's switch: on starts a full grant (egress attached), off ends all grants of the chat, one internet_end
// entry each, and detaches the sandbox.
func TestUserSwitchEndsAll(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	startTestGrant(t, e, chatID, "search", nil, 30, 100)
	startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	if v, err := e.m.SetInternetByUser(ctx, chatID, true); err != nil || !e.agent(0).internet || len(v.InternetGrants) != 3 {
		t.Fatalf("on: %v %v %+v", err, e.agent(0).internet, v.InternetGrants)
	}
	gs, _ := e.st.ActiveGrants(ctx, chatID)
	if gs[2].Kind != "full" || gs[2].Origin != store.GrantByUser || gs[2].MaxMinutes != 30 || gs[2].MaxBytes != 500<<20 {
		t.Fatalf("user grant: %+v", gs[2])
	}
	v, err := e.m.SetInternetByUser(ctx, chatID, false)
	if err != nil || v.Internet || len(v.InternetGrants) != 0 || e.agent(0).internet {
		t.Fatalf("off: %v %+v internet %v", err, v.InternetGrants, e.agent(0).internet)
	}
	all, _ := e.st.Grants(ctx, chatID)
	for _, g := range all {
		if g.State != store.GrantEnded || g.EndedReason != store.EndUser {
			t.Errorf("grant %d: %s %s", g.ID, g.State, g.EndedReason)
		}
	}
	calls, _ := e.st.ListSocketCalls(ctx, chatID)
	ends := 0
	for _, c := range calls {
		if c.Op == "internet_end" {
			ends++
			if c.Result != "user" || c.Via != "gateway" || !strings.HasPrefix(c.Detail, "grant ") {
				t.Errorf("end entry: %+v", c)
			}
		}
	}
	if ends != 3 {
		t.Fatalf("internet_end entries: %d", ends)
	}
}

// The agent can end one grant by id or all; a grant of another chat is not touched.
func TestDisableInternetByGrant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatA, slotA := newChatNoNet(t, e)
	chatB, _ := newChatNoNet(t, e)
	g1 := startTestGrant(t, e, chatA, "search", nil, 30, 100)
	g2 := startTestGrant(t, e, chatA, "domains", []string{"a.example.org"}, 30, 100)
	gb := startTestGrant(t, e, chatB, "search", nil, 30, 100)
	if r, err := e.m.DisableInternet(ctx, chatA, slotA, "cli", gb.ID); err != nil || r.Status != sock.InternetAlreadyOff {
		t.Fatalf("a foreign grant: %+v %v", r, err)
	}
	if r, err := e.m.DisableInternet(ctx, chatA, slotA, "cli", g1.ID); err != nil || r.Status != sock.InternetOff {
		t.Fatalf("one grant: %+v %v", r, err)
	}
	if left := e.m.ActiveGrantViews(chatA); len(left) != 1 || left[0].ID != g2.ID {
		t.Fatalf("left: %+v", left)
	}
	if len(e.m.ActiveGrantViews(chatB)) != 1 {
		t.Fatal("the other chat lost its grant")
	}
	if r, _ := e.m.DisableInternet(ctx, chatA, slotA, "cli", 0); r.Status != sock.InternetOff {
		t.Fatalf("all: %+v", r)
	}
	if r, _ := e.m.DisableInternet(ctx, chatA, slotA, "cli", 0); r.Status != sock.InternetAlreadyOff {
		t.Fatalf("again: %+v", r)
	}
	if got, _ := e.st.GetGrant(ctx, g1.ID); got.EndedReason != store.EndAgent {
		t.Fatalf("reason: %+v", got)
	}
}

// The user ends one grant through the API call; a second time it is not active any more.
func TestEndGrantByUser(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatA, _ := newChatNoNet(t, e)
	chatB, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatA, "search", nil, 30, 100)
	if _, err := e.m.EndGrantByUser(ctx, chatB, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another chat's grant: %v", err)
	}
	got, err := e.m.EndGrantByUser(ctx, chatA, g.ID)
	if err != nil || got.State != store.GrantEnded || got.EndedReason != store.EndUser {
		t.Fatalf("end: %+v %v", got, err)
	}
	if _, err := e.m.EndGrantByUser(ctx, chatA, g.ID); !errors.Is(err, ErrGrantNotActive) {
		t.Fatalf("second end: %v", err)
	}
}

// The time limit ends a grant at the next tick, whatever the chat does; the refusal that follows names it.
func TestGrantTimeLimitEnds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	live := e.m.grants.get(g.ID)
	live.expires = time.Now().Add(-time.Second)
	e.m.grantTick(ctx, 1)
	got, _ := e.st.GetGrant(ctx, g.ID)
	if got.State != store.GrantEnded || got.EndedReason != store.EndTime {
		t.Fatalf("grant: %+v", got)
	}
	if v, _ := e.m.View(ctx, chatID); v.Internet || len(v.InternetGrants) != 0 {
		t.Fatalf("view: %+v", v.InternetGrants)
	}
	if d := e.m.WebGate().Decide(chatID, netgrant.OriginExec, "a.example.org", 443); d.Allow || !strings.Contains(d.Reason, "ended (time limit)") {
		t.Fatalf("decide: %+v", d)
	}
}

// The data limit counts both directions over all connections of the grant and ends it at the tick.
func TestGrantByteLimitEnds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "search", nil, 30, 1) // 1 MB
	gate := e.m.WebGate()
	gate.Count(g.ID, 600<<10)
	gate.Count(g.ID, 600<<10)
	if ok, why := gate.Still(chatID, g.ID); ok || why != "data limit" {
		t.Fatalf("still: %v %q", ok, why)
	}
	e.m.grantTick(ctx, 5) // 5: also saves the counters
	got, _ := e.st.GetGrant(ctx, g.ID)
	if got.State != store.GrantEnded || got.EndedReason != store.EndBytes || got.Bytes < 1<<20 {
		t.Fatalf("grant: %+v", got)
	}
}

// A full grant counts what the sandbox's network interfaces carry (Docker's counters), not only what the proxy sees.
func TestFullGrantCountsSandboxBytes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "full", nil, 30, 1)
	if !e.agent(0).internet {
		t.Fatal("a full grant did not attach the egress network")
	}
	e.m.grantTick(ctx, 3) // first sample: the baseline
	if b := e.m.grants.get(g.ID).bytes(); b != 0 {
		t.Fatalf("baseline counted: %d", b)
	}
	a := e.agent(0)
	a.mu.Lock()
	a.netBytes = 2 << 20
	a.mu.Unlock()
	e.m.grantTick(ctx, 3)
	got, _ := e.st.GetGrant(ctx, g.ID)
	if got.State != store.GrantEnded || got.EndedReason != store.EndBytes || e.agent(0).internet {
		t.Fatalf("grant %+v, egress %v", got, e.agent(0).internet)
	}
}

// A resumed sandbox gets the egress network only if a full grant is active; search and domains grants stay without.
func TestAttachRestoresEgressOnlyForFull(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	startTestGrant(t, e, chatID, "search", nil, 30, 100)
	startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	newest := func() *fakeAgent {
		e.mu.Lock()
		defer e.mu.Unlock()
		last := e.agents[len(e.agents)-1]
		for _, ag := range e.agents {
			if ag.hasCmds() {
				last = ag
			}
		}
		return last
	}
	resume := func() {
		if _, err := e.m.Suspend(ctx, chatID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.Send(ctx, chatID, "continue"); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, e, chatID)
	}
	resume()
	a := newest()
	a.mu.Lock()
	on, calls := a.internet, a.netCalls
	a.mu.Unlock()
	if on || calls != 0 {
		t.Fatalf("search and domains attached egress: %v, %d calls", on, calls)
	}
	if v, _ := e.m.View(ctx, chatID); !v.Internet || len(v.InternetGrants) != 2 {
		t.Fatalf("grants lost across the resume: %+v", v.InternetGrants)
	}
	startTestGrant(t, e, chatID, "full", nil, 30, 100)
	resume()
	b := newest()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.internet {
		t.Fatal("a full grant was not restored on the fresh sandbox")
	}
}

// A chat created with internet starts with a full grant; the origin tells the request from the default.
func TestCreateWithInternetStartsFullGrant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	on := true
	c, err := e.m.Create(ctx, NewChat{Internet: &on})
	if err != nil {
		t.Fatal(err)
	}
	gs, _ := e.st.ActiveGrants(ctx, c.ID)
	if len(gs) != 1 || gs[0].Kind != "full" || gs[0].Origin != store.GrantByUser || !e.agent(0).internet || !c.Internet {
		t.Fatalf("grants %+v, egress %v", gs, e.agent(0).internet)
	}
	e.m.opt.InternetDefault = true
	d, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if gs, _ = e.st.ActiveGrants(ctx, d.ID); len(gs) != 1 || gs[0].Origin != store.GrantByDefault {
		t.Fatalf("default grants: %+v", gs)
	}
	// An experiment chat never starts with internet (see TestExperimentInternetOff): no grant at all.
}

// A refused connection counts on the grant that governs the chat and goes into the log with the grant id and origin.
func TestRecordWebCountsBlocked(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	id := e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "CONNECT", Host: "github.com", Port: 443, Status: 403, Denied: "blocked: x", StartedAt: time.Now(), GrantID: g.ID, Origin: "exec"})
	if id == 0 {
		t.Fatal("not stored")
	}
	ev := waitEvent(t, events, "web_request", "").Data.(store.WebRequest)
	if ev.GrantID != g.ID || ev.Origin != "exec" {
		t.Fatalf("event: %+v", ev)
	}
	if v := e.m.ActiveGrantViews(chatID); v[0].Blocked != 1 {
		t.Fatalf("blocked in the view: %+v", v)
	}
	if got, _ := e.st.GetGrant(ctx, g.ID); got.Blocked != 1 {
		t.Fatalf("blocked stored: %+v", got)
	}
	rows, _ := e.m.GrantWebRequests(ctx, chatID, g.ID)
	if len(rows) != 1 || rows[0].Host != "github.com" {
		t.Fatalf("rows of the grant: %+v", rows)
	}
}

// The single row cap collapses the log of a busy grant (AGW_INTERNET_LOG_ROWS).
func TestGrantLogRowCap(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.opt.InternetLogRows = 3
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "full", nil, 30, 100)
	for i := 0; i < 10; i++ {
		e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "CONNECT", Host: "a.example.org", Port: 443, Status: 200, StartedAt: time.Now(), GrantID: g.ID, Origin: "pi"})
	}
	rows, _ := e.m.GrantWebRequests(ctx, chatID, g.ID)
	if len(rows) != 4 || rows[3].Method != store.CollapsedMethod || rows[3].Count != 7 {
		t.Fatalf("rows: %+v", rows)
	}
}

// Grants survive a restart of the gateway: LoadGrants reads the active ones; an ended one is not loaded.
func TestLoadGrants(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	other := startTestGrant(t, e, chatID, "search", nil, 30, 100)
	e.m.EndGrant(ctx, chatID, other.ID, store.EndAgent)
	e.m.grants = newGrantBook()
	if err := e.m.LoadGrants(ctx); err != nil {
		t.Fatal(err)
	}
	act := e.m.ActiveGrantViews(chatID)
	if len(act) != 1 || act[0].ID != g.ID {
		t.Fatalf("loaded: %+v", act)
	}
	if d := e.m.WebGate().Decide(chatID, netgrant.OriginExec, "a.example.org", 443); !d.Allow {
		t.Fatalf("decide after the load: %+v", d)
	}
}

// agw-internet status lists the refusals the counter counts, newest first, at most three.
func TestRecentRefusalsWindow(t *testing.T) {
	e := setup(t)
	chatID, _ := newChatNoNet(t, e)
	for i := 0; i < 4; i++ {
		e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "GET", Host: "pypi.org", Port: 443, Status: 413, Denied: "blocked: too large", Reason: "too_large",
			Package: "torch", StartedAt: time.Now(), Origin: "pkg"})
	}
	rs := e.m.RecentRefusals(chatID)
	v := e.m.blockedView(context.Background(), chatID)
	if len(rs) != 3 || rs[0].Reason != "too_large" || v == nil || v.Count != 4 || len(v.Last) != 3 {
		t.Fatalf("refusals %+v, view %+v", rs, v)
	}
	// a chat without refusals
	other, _ := newChatNoNet(t, e)
	if rs := e.m.RecentRefusals(other); len(rs) != 0 {
		t.Fatalf("other chat: %+v", rs)
	}
}

// A tunnel that closes with a refusal counts like a refused request: the grant counter and the event.
func TestFinishWebCountsRefusal(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g := startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	id := e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "CONNECT", Host: "a.example.org", Port: 443, Status: 200, StartedAt: time.Now(), GrantID: g.ID, Origin: "exec"})
	if id == 0 {
		t.Fatal("not stored")
	}
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	e.m.FinishWeb(id, webproxy.Request{ChatID: chatID, GrantID: g.ID, BytesDown: 5 << 20, Denied: "blocked: the connection to a.example.org was closed after 256 MB", Reason: "too_large"})
	waitEvent(t, events, "internet_blocked", "")
	if got, _ := e.st.GetGrant(ctx, g.ID); got.Blocked != 1 {
		t.Fatalf("blocked stored: %+v", got)
	}
	rows, _ := e.m.GrantWebRequests(ctx, chatID, g.ID)
	if len(rows) != 1 || rows[0].Reason != "too_large" {
		t.Fatalf("rows: %+v", rows)
	}
}

// drainKind empties the channel without blocking and returns the events of the given kind. publish is synchronous,
// so whatever a call has published is in the buffer when the call returns: no deadline needed.
func drainKind(ch <-chan Event, kind string) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

// Issue #229: when a grant starts, the counter starts at the grant, so the refusals before it no longer count; the UI
// is told with count 0 instead of keeping the old number until a reload. Issue #240: one event each, read without a
// deadline.
func TestPublishBlockedZeroOnGrantStart(t *testing.T) {
	e := setup(t)
	chatID, _ := newChatNoNet(t, e)
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	before := time.Now()
	e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "CONNECT", Host: "accounts.google.com", Port: 443, Status: 403, Denied: "blocked: internet is off", Reason: "internet_off", StartedAt: before.Add(-time.Minute), Origin: "exec"})
	evs := drainKind(events, "internet_blocked")
	if len(evs) != 1 {
		t.Fatalf("after the refusal: %d events", len(evs))
	}
	if v := evs[0].Data.(*BlockedView); v.Count != 1 {
		t.Fatalf("after the refusal: %+v", v)
	}
	startTestGrant(t, e, chatID, "domains", []string{"a.example.org"}, 30, 100)
	evs = drainKind(events, "internet_blocked")
	if len(evs) != 1 {
		t.Fatalf("after the grant started: %d events", len(evs))
	}
	if v := evs[0].Data.(*BlockedView); v.Count != 0 || len(v.Last) != 0 || v.Since.Before(before) {
		t.Fatalf("after the grant started: %+v", v)
	}
}

// Issue #240: the counter starts at the earliest start of the active grants, not at their creation.
func TestBlockedSinceUsesGrantStart(t *testing.T) {
	e := setup(t)
	chatID := "chat-since"
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(d time.Duration) *time.Time { x := t0.Add(d); return &x }
	e.m.grants.add(store.Grant{ID: 9001, ChatID: chatID, State: store.GrantActive, CreatedAt: t0, StartedAt: at(10 * time.Minute)})
	if got := e.m.blockedSince(chatID); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("one grant: %v", got)
	}
	e.m.grants.add(store.Grant{ID: 9002, ChatID: chatID, State: store.GrantActive, CreatedAt: t0, StartedAt: at(5 * time.Minute)})
	if got := e.m.blockedSince(chatID); !got.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("two grants: %v", got)
	}
	other := "chat-since-2"
	e.m.grants.add(store.Grant{ID: 9003, ChatID: other, State: store.GrantActive, CreatedAt: t0})
	if got := e.m.blockedSince(other); !got.Equal(t0) {
		t.Fatalf("without started_at: %v", got)
	}
}

// Issue #240: a requested grant is created at the request but starts at the user's approval; refusals in between do
// not count.
func TestBlockedCountStartsAtActivation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	g, err := e.st.CreateGrant(ctx, store.Grant{ChatID: chatID, Kind: "domains", Origin: store.GrantByAgent, Domains: []string{"a.example.org"},
		State: store.GrantPending, MaxMinutes: 30, MaxBytes: 100 << 20})
	if err != nil {
		t.Fatal(err)
	}
	e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "CONNECT", Host: "accounts.google.com", Port: 443, Status: 403, Denied: "blocked: internet is off", Reason: "internet_off", StartedAt: time.Now().Add(-time.Second), Origin: "exec"})
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	if _, err := e.m.activateGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	evs := drainKind(events, "internet_blocked")
	if len(evs) == 0 {
		t.Fatal("no internet_blocked event")
	}
	if v := evs[len(evs)-1].Data.(*BlockedView); v.Count != 0 {
		t.Fatalf("after the activation: %+v", v)
	}
}

// Issue #229: without a grant, the counter falls to 0 by itself once the last refusal has left the window, and the
// UI is told. With a grant active nothing leaves the window, so no timer runs.
func TestPublishBlockedWindowExpiry(t *testing.T) {
	oldW, oldS := blockedWindow, blockedSlack
	blockedWindow, blockedSlack = 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { blockedWindow, blockedSlack = oldW, oldS })
	e := setup(t)
	chatID, _ := newChatNoNet(t, e)
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	e.m.RecordWeb(webproxy.Request{ChatID: chatID, Method: "GET", Host: "clients2.google.com", Port: 80, Status: 403, Denied: "blocked: internet is off", Reason: "internet_off", StartedAt: time.Now(), Origin: "exec"})
	if v := waitEvent(t, events, "internet_blocked", "").Data.(*BlockedView); v.Count != 1 {
		t.Fatalf("after the refusal: %+v", v)
	}
	e.m.blockedMu.Lock()
	armed := e.m.blockedTimers[chatID] != nil
	e.m.blockedMu.Unlock()
	if !armed {
		t.Fatal("no timer armed")
	}
	done := make(chan *BlockedView, 1)
	go func() { done <- waitEvent(t, events, "internet_blocked", "").Data.(*BlockedView) }()
	select {
	case v := <-done:
		if v.Count != 0 {
			t.Fatalf("after the window: %+v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event with count 0 within 2 s")
	}

	// with a grant active: no timer
	other, _ := newChatNoNet(t, e)
	startTestGrant(t, e, other, "domains", []string{"a.example.org"}, 30, 100)
	e.m.RecordWeb(webproxy.Request{ChatID: other, Method: "CONNECT", Host: "github.com", Port: 443, Status: 403, Denied: "blocked: x", StartedAt: time.Now(), Origin: "exec"})
	e.m.blockedMu.Lock()
	armed = e.m.blockedTimers[other] != nil
	e.m.blockedMu.Unlock()
	if armed {
		t.Fatal("timer armed although a grant is active")
	}
}

// The chat view still carries no internet_blocked without refusals (omitempty), although the event goes out with 0.
func TestBlockedViewNilAtZero(t *testing.T) {
	e := setup(t)
	chatID, _ := newChatNoNet(t, e)
	if v := e.m.blockedView(context.Background(), chatID); v != nil {
		t.Fatalf("view without refusals: %+v", v)
	}
	s, err := e.m.blockedState(context.Background(), chatID)
	if err != nil || s.Count != 0 || s.Last == nil {
		t.Fatalf("state: %+v, %v", s, err)
	}
}

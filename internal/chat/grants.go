// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agw/internal/netgrant"
	"agw/internal/platform"
	"agw/internal/sock"
	"agw/internal/store"
	"agw/internal/webproxy"
)

// Internet grants (issue #175). A chat holds any number of grants (at most netgrant.MaxGrants active); what it may
// reach is their union. The database is the record, the manager keeps the active ones in memory (grantBook) because
// the web proxy asks on every connection and counts bytes on every read.

// grantLive is an active grant with its running counters.
type grantLive struct {
	g        store.Grant
	proxy    atomic.Int64 // bytes counted at the web proxy (both directions)
	blocked  atomic.Int64
	maxBytes int64
	expires  time.Time
	// Docker's byte counters of the execution sandbox for a full grant (the proxy only sees what goes through it).
	netMu   sync.Mutex
	netSeen bool
	netLast int64
	netSum  int64
	// pkgs: the allowed packages of a packages grant (nil for other kinds).
	pkgs *pkgSet
	// persisted/published: the last values written to the database and sent to the UI.
	persisted, published int64
}

func (l *grantLive) bytes() int64 {
	l.netMu.Lock()
	n := l.netSum
	l.netMu.Unlock()
	return l.proxy.Load() + n
}

// over reports whether a limit is reached: "time", "bytes" or "".
func (l *grantLive) over(now time.Time) string {
	switch {
	case !l.expires.IsZero() && !now.Before(l.expires):
		return store.EndTime
	case l.maxBytes > 0 && l.bytes() >= l.maxBytes:
		return store.EndBytes
	}
	return ""
}

// sample adds the growth of the sandbox's network counters (a smaller value means a new container).
func (l *grantLive) sample(nb int64) {
	l.netMu.Lock()
	defer l.netMu.Unlock()
	switch {
	case !l.netSeen:
		l.netSeen = true
	case nb >= l.netLast:
		l.netSum += nb - l.netLast
	default:
		l.netSum += nb
	}
	l.netLast = nb
}

func (l *grantLive) resetSample() {
	l.netMu.Lock()
	l.netSeen = false
	l.netMu.Unlock()
}

// view is the grant as the UI and the API show it, with the live counters.
func (l *grantLive) view() store.Grant {
	g := l.g
	if l.pkgs != nil {
		g.Packages = packageStates(l.pkgs.list())
	}
	g.Bytes = max(g.Bytes, l.bytes())
	g.Blocked = max(g.Blocked, int(l.blocked.Load()))
	return g
}

// endedGrant remembers the last grant that ended in a chat, so that a refusal right after names it.
type endedGrant struct {
	id     int64
	reason string
	at     time.Time
}

type grantBook struct {
	mu        sync.Mutex
	byID      map[int64]*grantLive
	byChat    map[string][]*grantLive
	lastEnded map[string]endedGrant
}

func newGrantBook() *grantBook {
	return &grantBook{byID: map[int64]*grantLive{}, byChat: map[string][]*grantLive{}, lastEnded: map[string]endedGrant{}}
}

func (b *grantBook) add(g store.Grant) *grantLive {
	l := &grantLive{g: g, maxBytes: g.MaxBytes}
	if g.ExpiresAt != nil {
		l.expires = *g.ExpiresAt
	}
	l.proxy.Store(g.Bytes)
	l.persisted, l.published = g.Bytes, g.Bytes
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.byID[g.ID]; old != nil {
		return old
	}
	b.byID[g.ID] = l
	b.byChat[g.ChatID] = append(b.byChat[g.ChatID], l)
	return l
}

func (b *grantBook) remove(id int64, reason string) *grantLive {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.byID[id]
	if l == nil {
		return nil
	}
	delete(b.byID, id)
	list := b.byChat[l.g.ChatID]
	for i, x := range list {
		if x == l {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(b.byChat, l.g.ChatID)
	} else {
		b.byChat[l.g.ChatID] = list
	}
	b.lastEnded[l.g.ChatID] = endedGrant{id: id, reason: reason, at: time.Now()}
	return l
}

func (b *grantBook) get(id int64) *grantLive {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byID[id]
}

func (b *grantBook) active(chat string) []*grantLive {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*grantLive(nil), b.byChat[chat]...)
}

func (b *grantBook) all() []*grantLive {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*grantLive, 0, len(b.byID))
	for _, l := range b.byID {
		out = append(out, l)
	}
	return out
}

func (b *grantBook) ended(chat string) (endedGrant, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.lastEnded[chat]
	return e, ok
}

// grantTick is the period of the maintenance loop: limits, progress events, saving the counters.
const grantTick = 2 * time.Second

// InternetState tells which of pi's web tools are on offer: any grant at all, web_search (search or full) and
// web_extract (search, domains or full). The proxy enforces it; this only controls what the model is shown.
func (m *Manager) InternetState(chatID string) (enabled, search, extract bool) {
	for _, l := range m.grants.active(chatID) {
		enabled = true
		switch netgrant.Kind(l.g.Kind) {
		case netgrant.Search:
			search, extract = true, true
		case netgrant.Domains:
			extract = true
		case netgrant.Full:
			search, extract = true, true
		}
	}
	return
}

// ActiveGrantViews returns the chat's active grants with live counters, oldest first.
func (m *Manager) ActiveGrantViews(chatID string) []store.Grant {
	ls := m.grants.active(chatID)
	out := make([]store.Grant, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.view())
	}
	return out
}

// InternetGrants lists a chat's grants, newest first, with the live counters of the active ones.
func (m *Manager) InternetGrants(ctx context.Context, chatID string) ([]store.Grant, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return nil, err
	}
	gs, err := m.st.Grants(ctx, chatID)
	if err != nil {
		return nil, err
	}
	for i := range gs {
		if l := m.grants.get(gs[i].ID); l != nil {
			gs[i] = l.view()
		} else if gs[i].Kind == string(netgrant.Packages) {
			if gps, err := m.st.GrantPackages(ctx, gs[i].ID); err == nil {
				gs[i].Packages = packageStates(gps)
			}
		}
	}
	return gs, nil
}

// GrantWebRequests lists the connections of one grant of a chat.
func (m *Manager) GrantWebRequests(ctx context.Context, chatID string, grantID int64) ([]store.WebRequest, error) {
	if _, err := m.st.ChatGrant(ctx, chatID, grantID); err != nil {
		return nil, err
	}
	return m.st.GrantWebRequests(ctx, chatID, grantID)
}

// WebConnections lists connections across chats for the activity (the owner filter is the caller's).
func (m *Manager) WebConnections(ctx context.Context, f store.WebConnectionFilter) ([]store.WebRequest, error) {
	return m.st.WebConnections(ctx, f)
}

// ActivityChats returns title, model and binding of the chats a connection list refers to.
func (m *Manager) ActivityChats(ctx context.Context, ids []string) (map[string]store.ActivityChat, error) {
	return m.st.ActivityChats(ctx, ids)
}

// limits returns the configured limits of the grants.
func (m *Manager) limits() netgrant.Limits { return m.opt.InternetLimits }

// internetLogRows is the number of single rows per grant in web_requests before collapsing.
func (m *Manager) internetLogRows() int {
	if m.opt.InternetLogRows > 0 {
		return m.opt.InternetLogRows
	}
	return DefaultInternetLogRows
}

// DefaultInternetLogRows: single rows per grant in web_requests (AGW_INTERNET_LOG_ROWS).
const DefaultInternetLogRows = 500

func (m *Manager) publishGrant(l *grantLive) {
	v := l.view()
	l.published = v.Bytes
	m.publish(v.ChatID, Event{Kind: "internet_grant", Data: v})
}

// syncInternetFlag derives chats.internet ("any grant active") from the book and tells the UI.
func (m *Manager) syncInternetFlag(ctx context.Context, chatID string) {
	on := len(m.grants.active(chatID)) > 0
	if cur, err := m.st.ChatInternet(ctx, chatID); err == nil && cur != on {
		if err := m.st.SetInternet(ctx, chatID, on); err != nil {
			slog.Warn("internet flag not stored", "chat", chatID, "error", err)
		}
	}
}

// egressWanted: does an active full grant of the chat need the sandbox's egress network?
func (m *Manager) egressWanted(chatID string) bool {
	now := time.Now()
	for _, l := range m.grants.active(chatID) {
		if l.g.Kind == string(netgrant.Full) && l.over(now) == "" {
			return true
		}
	}
	return false
}

// syncEgress brings the execution sandbox's egress network in line with the full grants. The caller holds the chat
// lock. A failure to detach is returned: a remaining route out would be a way past the grants.
func (m *Manager) syncEgress(ctx context.Context, chatID string) error {
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l == nil {
		return nil
	}
	want := m.egressWanted(chatID)
	m.mu.Lock()
	cur := l.egress
	m.mu.Unlock()
	if cur == want {
		return nil
	}
	if err := l.slot.Worker.SetInternet(ctx, want); err != nil {
		return err
	}
	m.mu.Lock()
	l.egress = want
	m.mu.Unlock()
	if want {
		for _, g := range m.grants.active(chatID) {
			g.resetSample()
		}
	}
	return nil
}

// --- the web proxy's view (webproxy.Gate) ---

// webGate is the manager as the web proxy's gate (webproxy.Gate). It is a separate type because the manager already
// has Decide (approvals) and Attribute (subagents); the embedded manager supplies RecordWeb and FinishWeb.
type webGate struct{ *Manager }

// WebGate returns the gate for the web proxy.
func (m *Manager) WebGate() webproxy.Gate { return webGate{m} }

// Attribute assigns a request at the web proxy to its chat by the address of pi's container in the slot network,
// as at the LLM proxy.
func (g webGate) Attribute(ip string) (chat, slot string) {
	if ip == "" {
		return "", ""
	}
	m := g.Manager
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, l := range m.live {
		if l.ip == ip {
			return id, l.slot.ID
		}
	}
	return "", ""
}

// AttributeSlot assigns a request that arrives at the socket of a slot (the sandbox's side) to its chat.
func (m *Manager) AttributeSlot(slotID string) (chat, slot string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, l := range m.live {
		if l.slot.ID == slotID {
			return id, slotID
		}
	}
	return "", ""
}

// Decide checks a connection against the chat's active grants. Grants over their limit do not count;
// they are ended right away instead of at the next tick.
func (g webGate) Decide(chat string, o netgrant.Origin, host string, port int) netgrant.Decision {
	m := g.Manager
	now := time.Now()
	var act []netgrant.Active
	for _, l := range m.grants.active(chat) {
		if why := l.over(now); why != "" {
			go m.EndGrant(context.Background(), chat, l.g.ID, why)
			continue
		}
		act = append(act, netgrant.Active{ID: l.g.ID, Kind: netgrant.Kind(l.g.Kind), Domains: l.g.Domains})
	}
	if len(act) == 0 {
		if e, ok := m.grants.ended(chat); ok && time.Since(e.at) < 5*time.Minute {
			return netgrant.Decision{Reason: netgrant.MsgGrantEnded(e.id, netgrant.EndWhy(e.reason)), Code: netgrant.CodeAccessEnded}
		}
	}
	return netgrant.Decide(act, o, host, port)
}

// Still reports whether a grant still lets traffic through (webproxy.Gate).
func (g webGate) Still(chat string, grant int64) (bool, string) {
	m := g.Manager
	l := m.grants.get(grant)
	if l == nil || l.g.ChatID != chat {
		if e, ok := m.grants.ended(chat); ok && e.id == grant {
			return false, netgrant.EndWhy(e.reason)
		}
		return false, netgrant.EndWhy("")
	}
	if why := l.over(time.Now()); why != "" {
		go m.EndGrant(context.Background(), chat, grant, why)
		return false, netgrant.EndWhy(why)
	}
	return true, ""
}

// Count adds bytes to the grant's counter (webproxy.Gate).
func (g webGate) Count(grant, n int64) {
	m := g.Manager
	if l := m.grants.get(grant); l != nil && n > 0 {
		l.proxy.Add(n)
	}
}

// --- requests, activation, ends ---

// internetCard is the structured data of an internet_access approval (store.Approval.Internet).
type internetCard struct {
	GrantID           int64    `json:"grant_id"`
	Kind              string   `json:"kind"`
	Domains           []string `json:"domains"`
	Packages          []string `json:"packages"`
	Minutes           int      `json:"minutes"`
	Megabytes         int      `json:"megabytes"`
	EndsWhenInstalled bool     `json:"ends_when_installed"`
	// By: who asks: the main agent or a helper (a subagent). ExpiresAt: when the request lapses undecided.
	By        string    `json:"by"`
	ExpiresAt time.Time `json:"expires_at"`
}

// cardText is the readable form of the grant for clients that do not know the structured data yet: what the
// gateway enforces, then the agent's note marked as unverified.
func cardText(c internetCard, reason string) string {
	var what string
	switch netgrant.Kind(c.Kind) {
	case netgrant.Search:
		what = "Search only: web_search and web_extract"
	case netgrant.Domains:
		what = "Domains (https only): " + strings.Join(c.Domains, ", ")
	case netgrant.Packages:
		what = "Packages: " + strings.Join(c.Packages, ", ")
	default:
		what = "Full internet access for commands and tools (no list)"
	}
	return fmt.Sprintf("%s\nUp to %d min or %d MB, whichever comes first.\nAgent's note (not verified): %s", what, c.Minutes, c.Megabytes, reason)
}

func bytesOf(mb int) int64 { return int64(mb) << 20 }

// RequestInternet: the agent asks for internet access of one kind. The call waits for the user's decision; on
// approval a grant starts (issue #175). Only the narrow kinds are available through the bindings without a shell
// (via mcp and api); packages are not available yet.
func (m *Manager) RequestInternet(ctx context.Context, chatID, slotID, via string, spec netgrant.Spec) (sock.UploadResult, error) {
	refuse := func(msg string) (sock.UploadResult, error) {
		return sock.UploadResult{Status: "refused", Name: "internet", Message: msg, Log: "refused: " + msg}, nil
	}
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return sock.UploadResult{}, err
	}
	if c.TrashedAt != nil {
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: platform.RefusedPrefix + trashRefusal, Log: "rejected: " + trashRefusal}, nil
	}
	spec.Reason = strings.TrimSpace(strings.ToValidUTF8(spec.Reason, ""))
	if r := []rune(spec.Reason); len(r) > netgrant.MaxReason {
		spec.Reason = string(r[:netgrant.MaxReason]) + " …"
	}
	if spec.Reason == "" {
		spec.Reason = "(no reason given)"
	}
	spec, err = netgrant.Clamp(spec, m.limits())
	if err != nil {
		return refuse(err.Error())
	}
	if (via == "mcp" || via == "api") && spec.Kind != netgrant.Search && spec.Kind != netgrant.Domains {
		return refuse(fmt.Sprintf("kind %s %s: this binding has no shell, so only search and domains are useful here", spec.Kind, netgrant.MsgNeedsCommand))
	}
	// An equal grant that is active already answers the request.
	for _, l := range m.grants.active(chatID) {
		if sameGrant(l, spec) {
			return sock.UploadResult{Status: "approved", Name: "internet", Message: fmt.Sprintf("%s (grant %d, already active)", describeGrant(l.g), l.g.ID), Log: "already on"}, nil
		}
	}
	if n := len(m.grants.active(chatID)); n >= netgrant.MaxGrants {
		return refuse(fmt.Sprintf("this chat has %d active internet grants already; switch one off first (agw-internet off <grant id>)", n))
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return sock.UploadResult{}, err
	}
	var rootsJSON json.RawMessage
	if spec.Kind == netgrant.Packages {
		roots, err := netgrant.ParsePackages(spec.Packages)
		if err != nil {
			return refuse(err.Error())
		}
		rootsJSON, _ = json.Marshal(roots)
	}
	g, err := m.st.CreateGrant(ctx, store.Grant{ChatID: chatID, Kind: string(spec.Kind), Origin: store.GrantByAgent, Via: via,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx), Reason: spec.Reason, Domains: spec.Domains, Packages: rootsJSON, State: store.GrantPending,
		MaxMinutes: spec.Minutes, MaxBytes: bytesOf(spec.MB)})
	if err != nil {
		return sock.UploadResult{}, err
	}
	card := internetCard{GrantID: g.ID, Kind: string(spec.Kind), Domains: spec.Domains, Packages: spec.Packages, Minutes: spec.Minutes, Megabytes: spec.MB,
		EndsWhenInstalled: spec.Kind == netgrant.Packages, By: askerOf(ctx), ExpiresAt: time.Now().Add(m.opt.ApprovalTimeout)}
	if card.Domains == nil {
		card.Domains = []string{}
	}
	if card.Packages == nil {
		card.Packages = []string{}
	}
	data, _ := json.Marshal(card)
	ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "internet_access", Via: via, Name: spec.Reason, ContentType: "text/plain", PendingKey: "-",
		Preview: cardText(card, spec.Reason), Internet: data, Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		_, _ = m.st.CloseGrant(context.WithoutCancel(ctx), g.ID, store.GrantRejected)
		return sock.UploadResult{}, err
	}
	_ = m.st.SetGrantApproval(ctx, g.ID, ap.ID)
	w := m.broker.Register(ap.ID)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishChat(ctx, chatID)
	slog.Info("internet access requested", "chat", chatID, "approval", ap.ID, "grant", g.ID, "kind", spec.Kind, "via", via)
	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("tool", "")
	}
	switch final {
	case store.ApprovalExpired:
		_, _ = m.st.CloseGrant(bg, g.ID, store.GrantExpired)
		m.publishChat(bg, chatID)
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: "no decision within the waiting time", Log: "expired"}, nil
	case store.ApprovalApproved:
	default:
		_, _ = m.st.CloseGrant(bg, g.ID, store.GrantRejected)
		m.publishChat(bg, chatID)
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: "rejected by the user"}, nil
	}
	started, err := m.activateGrant(bg, g.ID)
	if err != nil {
		return sock.UploadResult{}, err
	}
	return sock.UploadResult{Status: "approved", Name: "internet", Message: fmt.Sprintf("%s (grant %d)", describeGrant(started), started.ID)}, nil
}

func sameGrant(l *grantLive, s netgrant.Spec) bool {
	g := l.g
	if g.Kind != string(s.Kind) {
		return false
	}
	switch s.Kind {
	case netgrant.Domains:
		have := map[string]bool{}
		for _, d := range g.Domains {
			have[d] = true
		}
		for _, d := range s.Domains {
			if !have[d] {
				return false
			}
		}
	case netgrant.Packages:
		roots, err := netgrant.ParsePackages(s.Packages)
		if err != nil || l.pkgs == nil {
			return false
		}
		for _, r := range roots {
			e, ok := l.pkgs.get(r.Ecosystem, r.Name)
			if !ok || e.Parent != "" || e.Version != r.Version {
				return false
			}
		}
	}
	return true
}

// askerOf tells who asks: the main agent, or a helper (a subagent) when the call comes from a subagent's session.
func askerOf(ctx context.Context) string {
	if s := store.SessionFrom(ctx); s != "" && s != "main" {
		return "helper"
	}
	return "agent"
}

// describeGrant is the line the agent reads: kind, list and end.
func describeGrant(g store.Grant) string {
	what := g.Kind
	if netgrant.Kind(g.Kind) == netgrant.Domains {
		what += " " + strings.Join(g.Domains, ", ")
	}
	if netgrant.Kind(g.Kind) == netgrant.Packages {
		var roots []netgrant.PkgRoot
		var names []string
		_ = json.Unmarshal(g.Packages, &roots)
		for _, r := range roots {
			names = append(names, r.String())
		}
		return fmt.Sprintf("packages %s until installed (at the latest %s or %d MB)", strings.Join(names, ", "), untilOf(g), g.MaxBytes>>20)
	}
	until := ""
	if g.ExpiresAt != nil {
		until = g.ExpiresAt.UTC().Format("15:04")
	}
	return fmt.Sprintf("%s until %s or %d MB", what, until, g.MaxBytes>>20)
}

func untilOf(g store.Grant) string {
	if g.ExpiresAt != nil {
		return g.ExpiresAt.UTC().Format("15:04")
	}
	return ""
}

// activateGrant starts a pending grant after the user's approval: the clock starts now.
func (m *Manager) activateGrant(ctx context.Context, id int64) (store.Grant, error) {
	g0, err := m.st.GetGrant(ctx, id)
	if err != nil {
		return store.Grant{}, err
	}
	unlock := m.lock(g0.ChatID)
	defer unlock()
	now := time.Now()
	g, ok, err := m.st.ActivateGrant(ctx, id, now, now.Add(time.Duration(g0.MaxMinutes)*time.Minute))
	if err != nil {
		return g, err
	}
	if !ok {
		return g, fmt.Errorf("grant %d is not pending any more", id)
	}
	return g, m.startGrant(ctx, g)
}

// startGrant puts an active grant into the book and gives it effect: egress for full, flag, events. The caller holds
// the chat lock.
func (m *Manager) startGrant(ctx context.Context, g store.Grant) error {
	l := m.grants.add(g)
	if g.Kind == string(netgrant.Packages) && l.pkgs == nil {
		m.initPackages(ctx, l, packageRoots(g))
	}
	if err := m.syncEgress(ctx, g.ChatID); err != nil {
		// No effect, no grant: end it again rather than leave a grant the sandbox does not have.
		m.endLocked(ctx, g.ChatID, g.ID, store.EndError)
		return fmt.Errorf("set internet: %w", err)
	}
	m.syncInternetFlag(ctx, g.ChatID)
	slog.Info("internet grant started", "chat", g.ChatID, "grant", g.ID, "kind", g.Kind, "origin", g.Origin)
	m.publishGrant(l)
	m.publishChat(ctx, g.ChatID)
	m.publishBlocked(g.ChatID) // the counter starts at the grant
	return nil
}

// EndGrant ends an active grant (reason store.EndTime … EndAgent); it reports whether the grant was active.
func (m *Manager) EndGrant(ctx context.Context, chatID string, id int64, reason string) bool {
	unlock := m.lock(chatID)
	defer unlock()
	return m.endLocked(context.WithoutCancel(ctx), chatID, id, reason)
}

func (m *Manager) endLocked(ctx context.Context, chatID string, id int64, reason string) bool {
	l := m.grants.get(id)
	var total int64
	if l != nil {
		total = l.bytes()
	}
	g, ok, err := m.st.EndGrant(ctx, id, reason, total)
	if err != nil {
		slog.Warn("internet grant not ended", "grant", id, "error", err)
	}
	m.grants.remove(id, reason)
	if l == nil && !ok {
		return false
	}
	if err := m.syncEgress(ctx, chatID); err != nil {
		slog.Error("egress not detached after a grant ended", "chat", chatID, "grant", id, "error", err)
	}
	m.syncInternetFlag(ctx, chatID)
	if l != nil {
		g = l.view()
		g.State, g.EndedReason, g.Bytes, g.Blocked = store.GrantEnded, reason, total, int(l.blocked.Load())
		now := time.Now()
		g.EndedAt = &now
		m.publish(chatID, Event{Kind: "internet_grant", Data: g})
	}
	slot := ""
	m.mu.Lock()
	if lv := m.live[chatID]; lv != nil {
		slot = lv.slot.ID
	}
	m.mu.Unlock()
	m.LogCallBy(ctx, slot, chatID, "gateway", "internet_end", fmt.Sprintf("grant %d %s", id, g.Kind), reason)
	slog.Info("internet grant ended", "chat", chatID, "grant", id, "kind", g.Kind, "reason", reason, "bytes", total)
	m.publishChat(ctx, chatID)
	m.publishBlocked(chatID) // the counter starts at the default window again
	return ok
}

// EndGrantByUser ends one grant on the user's request (POST /api/chats/{id}/internet/grants/{gid}/end); ErrGrantNotActive
// if it is not active in this chat.
func (m *Manager) EndGrantByUser(ctx context.Context, chatID string, id int64) (store.Grant, error) {
	g, err := m.st.ChatGrant(ctx, chatID, id)
	if err != nil {
		return store.Grant{}, err
	}
	if !m.EndGrant(ctx, chatID, id, store.EndUser) {
		return g, ErrGrantNotActive
	}
	return m.st.ChatGrant(ctx, chatID, id)
}

// ErrGrantNotActive: the grant is not active (any more).
var ErrGrantNotActive = errors.New("the internet grant is not active")

// EndAllGrants ends every active grant of the chat for the reason and returns the IDs of those that ended.
func (m *Manager) EndAllGrants(ctx context.Context, chatID, reason string) []int64 {
	var ended []int64
	for _, l := range m.grants.active(chatID) {
		if m.EndGrant(ctx, chatID, l.g.ID, reason) {
			ended = append(ended, l.g.ID)
		}
	}
	return ended
}

// startUserFull starts a full grant for the user's switch or the default of a new chat, with the default limits.
// The caller holds the chat lock.
func (m *Manager) startUserFull(ctx context.Context, chatID, origin, via string) error {
	_, err := m.startUserFullGrant(ctx, chatID, origin, via)
	return err
}

// startUserFullGrant is startUserFull returning the grant (the switch logs its ID).
func (m *Manager) startUserFullGrant(ctx context.Context, chatID, origin, via string) (store.Grant, error) {
	spec, _ := netgrant.Clamp(netgrant.Spec{Kind: netgrant.Full}, m.limits())
	now := time.Now()
	exp := now.Add(time.Duration(spec.Minutes) * time.Minute)
	g, err := m.st.CreateGrant(ctx, store.Grant{ChatID: chatID, Kind: string(netgrant.Full), Origin: origin, Via: via, State: store.GrantActive,
		MaxMinutes: spec.Minutes, MaxBytes: bytesOf(spec.MB), StartedAt: &now, ExpiresAt: &exp})
	if err != nil {
		return g, err
	}
	return g, m.startGrant(ctx, g)
}

// SetInternet runs under the chat lock. The old switch is now a full grant: on starts one with the default limits
// (unless a full grant is active), off ends all grants. Experiment chats never get internet.
func (m *Manager) SetInternet(ctx context.Context, chatID string, on bool) (ChatView, error) {
	return m.setInternet(ctx, chatID, on, false)
}

// SetInternetByUser is the user's switch (POST /api/chats/{id}/internet). Off ends all active grants; on starts a
// full grant of the user's with the default limits. Logged as a socket_calls entry with via "user" and op
// "internet_set" (result "on", "off", "already on" or "already off", issue #37). "already on" means a full grant
// was already active, not just some grant (issue #186).
func (m *Manager) SetInternetByUser(ctx context.Context, chatID string, on bool) (ChatView, error) {
	return m.setInternet(ctx, chatID, on, true)
}

// ViaUser marks the user's internet switch in the socket log (op store.OpInternetUser).
const ViaUser = "user"

func (m *Manager) setInternet(ctx context.Context, chatID string, on, byUser bool) (ChatView, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return ChatView{}, err
	}
	var switchedOn int64
	var ended []int64
	switch {
	case on:
		// hasFull and the grant creation share one lock, so a double click cannot start two full grants.
		unlock := m.lock(chatID)
		if !m.hasFull(chatID) {
			g, err := m.startUserFullGrant(ctx, chatID, store.GrantByUser, ViaUser)
			if err != nil {
				unlock()
				return ChatView{}, err
			}
			switchedOn = g.ID
		}
		unlock()
	case !on:
		reason := store.EndAgent
		if byUser {
			reason = store.EndUser
		}
		ended = m.EndAllGrants(ctx, chatID, reason)
	}
	slog.Info("internet toggled", "chat", chatID, "on", on, "by_user", byUser,
		"grant", switchedOn, "ended", len(ended))
	if byUser {
		// The result describes the effect: "on" only if this call started a full grant, "off" only if it ended one.
		result := "already on"
		if on && switchedOn > 0 {
			result = "on"
		} else if !on {
			result = "already off"
			if len(ended) > 0 {
				result = "off"
			}
		}
		slot := ""
		m.mu.Lock()
		if l := m.live[chatID]; l != nil {
			slot = l.slot.ID
		}
		m.mu.Unlock()
		detail := ""
		if switchedOn > 0 {
			detail = fmt.Sprintf("grant %d full", switchedOn)
		} else if !on {
			// The activity list summarises these ends under this entry (issue #185).
			detail = store.EndedGrantsDetail(ended)
		}
		m.LogCallBy(ctx, slot, chatID, ViaUser, store.OpInternetUser, detail, result)
	}
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

func (m *Manager) hasFull(chatID string) bool {
	for _, l := range m.grants.active(chatID) {
		if l.g.Kind == string(netgrant.Full) {
			return true
		}
	}
	return false
}

// DisableInternet: the agent ends one grant (grantID > 0) or all of its chat's grants. It only removes a right, so it
// needs no approval. Idempotent: if nothing is active, nothing changes and the result says so (status
// "already_off").
func (m *Manager) DisableInternet(ctx context.Context, chatID, slotID, via string, grantID int64) (sock.UploadResult, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return sock.UploadResult{}, err
	}
	bg := context.WithoutCancel(ctx)
	if grantID > 0 {
		l := m.grants.get(grantID)
		if l == nil || l.g.ChatID != chatID {
			return sock.UploadResult{Status: sock.InternetAlreadyOff, Name: "internet", Message: fmt.Sprintf("grant %d is not active", grantID)}, nil
		}
		m.EndGrant(bg, chatID, grantID, store.EndAgent)
		slog.Info("internet grant switched off by the agent", "chat", chatID, "slot", slotID, "via", via, "grant", grantID)
		return sock.UploadResult{Status: sock.InternetOff, Name: "internet", Message: fmt.Sprintf("grant %d switched off", grantID)}, nil
	}
	n := len(m.EndAllGrants(bg, chatID, store.EndAgent))
	if n == 0 {
		return sock.UploadResult{Status: sock.InternetAlreadyOff, Name: "internet", Message: "internet access was already off"}, nil
	}
	slog.Info("internet switched off by the agent", "chat", chatID, "slot", slotID, "via", via, "grants", n)
	return sock.UploadResult{Status: sock.InternetOff, Name: "internet", Message: "internet access switched off"}, nil
}

// --- maintenance ---

// LoadGrants reads the active grants from the database into the book (at the start).
func (m *Manager) LoadGrants(ctx context.Context) error {
	gs, err := m.st.AllActiveGrants(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		l := m.grants.add(g)
		if g.Kind == string(netgrant.Packages) {
			m.loadPackages(ctx, l)
		}
	}
	return nil
}

// RunInternetGrants ends grants at their limits, counts the bytes of full grants from Docker, saves the counters
// and reports progress to the UI, until ctx ends. LoadGrants has run before.
func (m *Manager) RunInternetGrants(ctx context.Context) {
	t := time.NewTicker(grantTick)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.grantTick(ctx, n)
		}
	}
}

func (m *Manager) grantTick(ctx context.Context, n int) {
	now := time.Now()
	for _, l := range m.grants.all() {
		chat := l.g.ChatID
		if l.g.Kind == string(netgrant.Full) && n%3 == 0 {
			m.sampleNet(ctx, chat, l)
		}
		if why := l.over(now); why != "" {
			m.EndGrant(ctx, chat, l.g.ID, why)
			continue
		}
		if b := l.bytes(); n%5 == 0 && b != l.persisted {
			if err := m.st.SetGrantBytes(ctx, l.g.ID, b); err == nil {
				l.persisted = b
			}
		}
		if b := l.bytes(); b != l.published {
			m.publishGrant(l)
		}
	}
	m.reconcileEgress(ctx)
}

func (m *Manager) sampleNet(ctx context.Context, chat string, l *grantLive) {
	m.mu.Lock()
	lv := m.live[chat]
	egress := lv != nil && lv.egress
	m.mu.Unlock()
	if !egress {
		return
	}
	if nb, err := lv.slot.Worker.NetBytes(ctx); err == nil {
		l.sample(nb)
	}
}

// reconcileEgress retries a detach that failed when the last grant ended.
func (m *Manager) reconcileEgress(ctx context.Context) {
	m.mu.Lock()
	var todo []string
	for id, l := range m.live {
		if l.egress && !m.egressWanted(id) {
			todo = append(todo, id)
		}
	}
	m.mu.Unlock()
	for _, id := range todo {
		unlock := m.lock(id)
		if err := m.syncEgress(ctx, id); err != nil {
			slog.Error("egress still attached", "chat", id, "error", err)
		}
		unlock()
	}
}

// --- refusals at the chat ---

// BlockedView is the chat's refusal counter: how many connections were refused since Since, and the latest ones.
type BlockedView struct {
	Count int                 `json:"count"`
	Since time.Time           `json:"since"`
	Last  []BlockedConnection `json:"last"`
}

// BlockedConnection is one refused connection in the counter's list.
type BlockedConnection struct {
	Host    string    `json:"host"`
	Origin  string    `json:"origin"`
	Reason  string    `json:"reason,omitempty"`
	Denied  string    `json:"denied"`
	Package string    `json:"package,omitempty"`
	Service string    `json:"service,omitempty"`
	At      time.Time `json:"at"`
}

// blockedWindow is how far back the counter looks when no grant is active; blockedSlack is added to the timer that
// sends the empty counter once the last refusal has left the window. Variables so that tests can shorten them.
var (
	blockedWindow = 15 * time.Minute
	blockedSlack  = time.Second
)

// blockedView reads the counter; nil if nothing was refused in the window.
func (m *Manager) blockedView(ctx context.Context, chatID string) *BlockedView {
	v, err := m.blockedState(ctx, chatID)
	if err != nil || v.Count == 0 {
		return nil
	}
	return v
}

// blockedState reads the counter; with nothing refused it is a view with count 0 (the UI clears its display).
func (m *Manager) blockedState(ctx context.Context, chatID string) (*BlockedView, error) {
	since := m.blockedSince(chatID)
	n, last, err := m.st.ChatBlocked(ctx, chatID, since, 3)
	if err != nil {
		slog.Warn("refusal counter not read", "chat", chatID, "error", err)
		return nil, err
	}
	v := &BlockedView{Count: n, Since: since, Last: []BlockedConnection{}}
	for _, r := range last {
		v.Last = append(v.Last, BlockedConnection{Host: r.Host, Origin: r.Origin, Reason: r.Reason, Denied: r.Denied, Package: r.Package, Service: r.Service, At: r.StartedAt})
	}
	return v, nil
}

// RecentRefusals: the last three refusals of the chat that the refusal counter counts (agw-internet status).
func (m *Manager) RecentRefusals(chatID string) []store.WebRequest {
	_, last, err := m.st.ChatBlocked(context.Background(), chatID, m.blockedSince(chatID), 3)
	if err != nil {
		return nil
	}
	return last
}

// blockedSince: where the refusal counter of a chat starts: the earliest start of the active grants (created_at only
// for rows without started_at), else the default window. started_at and the refusals' started_at both come from
// the gateway clock.
func (m *Manager) blockedSince(chatID string) time.Time {
	start := func(g store.Grant) time.Time {
		if g.StartedAt != nil {
			return *g.StartedAt
		}
		return g.CreatedAt
	}
	since := time.Now().Add(-blockedWindow)
	if act := m.grants.active(chatID); len(act) > 0 {
		since = start(act[0].g)
		for _, l := range act[1:] {
			if t := start(l.g); t.Before(since) {
				since = t
			}
		}
	}
	return since
}

// publishBlocked tells the UI the new state of the refusal counter, also when it is 0 (issue #229): the UI shows
// the count until told otherwise. Without a grant, a timer sends the 0 when the last refusal leaves the window.
func (m *Manager) publishBlocked(chatID string) {
	v, err := m.blockedState(context.Background(), chatID)
	if err != nil {
		return
	}
	m.publish(chatID, Event{Kind: "internet_blocked", Data: v})
	if v.Count > 0 && len(m.grants.active(chatID)) == 0 {
		m.armBlockedTimer(chatID)
	}
}

// armBlockedTimer (re)sets the chat's timer that publishes the counter again after the window has passed.
func (m *Manager) armBlockedTimer(chatID string) {
	m.blockedMu.Lock()
	defer m.blockedMu.Unlock()
	if m.blockedTimers == nil {
		m.blockedTimers = map[string]*time.Timer{}
	}
	if t := m.blockedTimers[chatID]; t != nil {
		t.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(blockedWindow+blockedSlack, func() {
		m.blockedMu.Lock()
		if m.blockedTimers[chatID] == t {
			delete(m.blockedTimers, chatID)
		}
		m.blockedMu.Unlock()
		m.publishBlocked(chatID)
	})
	m.blockedTimers[chatID] = t
}

// GrantBrief is what the activity chips need of a grant: its kind and what it lists.
type GrantBrief struct {
	Kind     string   `json:"kind"`
	Domains  []string `json:"domains"`
	Packages []string `json:"packages"`
}

// ConnectionExtras completes a page of connections: the totals over the whole filter (summary, total rows for
// "show N more"), the grants the rows refer to (chips), and the bytes full grants moved without being listed.
func (m *Manager) ConnectionExtras(ctx context.Context, f store.WebConnectionFilter, rows []store.WebRequest) (store.WebSummary, int, map[int64]GrantBrief, []store.UnlistedBytes, error) {
	sum, total, err := m.st.WebConnectionsSummary(ctx, f)
	if err != nil {
		return sum, 0, nil, nil, err
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, r := range rows {
		if r.GrantID > 0 && !seen[r.GrantID] {
			seen[r.GrantID] = true
			ids = append(ids, r.GrantID)
		}
	}
	gs, err := m.st.GrantsByID(ctx, ids)
	if err != nil {
		return sum, total, nil, nil, err
	}
	briefs := map[int64]GrantBrief{}
	for id, g := range gs {
		briefs[id] = briefOf(g)
	}
	var unlisted []store.UnlistedBytes
	if f.Result != "blocked" {
		if unlisted, err = m.st.FullGrantsUnlisted(ctx, f); err != nil {
			return sum, total, briefs, nil, err
		}
	}
	return sum, total, briefs, unlisted, nil
}

func briefOf(g store.Grant) GrantBrief {
	b := GrantBrief{Kind: g.Kind, Domains: g.Domains, Packages: g.RootStrings()}
	if b.Domains == nil {
		b.Domains = []string{}
	}
	return b
}

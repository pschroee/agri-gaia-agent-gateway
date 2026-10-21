// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"agw/internal/netgrant"
	"agw/internal/pkggate"
	"agw/internal/store"
	"agw/internal/webproxy"
)

// Packages grants (issue #175, stage 2). The allowed set of a grant is its roots plus the dependencies that allowed
// packages declare, found by the package gate in the metadata it passes on. The set lives in memory (the gate asks
// on every request) and is written through to internet_grant_packages.

// maxGrantPackages bounds the set of one grant: a package tree that large is not what the user approved.
const maxGrantPackages = 3000

type pkgSet struct {
	mu    sync.Mutex
	m     map[string]*store.GrantPackage
	order []string
}

func newPkgSet() *pkgSet { return &pkgSet{m: map[string]*store.GrantPackage{}} }

func pkgKey(eco, name string) string { return eco + ":" + name }

func (p *pkgSet) get(eco, name string) (store.GrantPackage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[pkgKey(eco, name)]
	if e == nil {
		return store.GrantPackage{}, false
	}
	return *e, true
}

// put adds a package unless it is known; known packages only gain extras. It reports whether the set changed.
func (p *pkgSet) put(gp store.GrantPackage) (added bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := pkgKey(gp.Ecosystem, gp.Name)
	if e := p.m[k]; e != nil {
		e.Extras = mergeExtras(e.Extras, gp.Extras)
		return false
	}
	if len(p.m) >= maxGrantPackages {
		return false
	}
	c := gp
	p.m[k] = &c
	p.order = append(p.order, k)
	return true
}

func (p *pkgSet) list() []store.GrantPackage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]store.GrantPackage, 0, len(p.order))
	for _, k := range p.order {
		out = append(out, *p.m[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return (out[i].Parent == "") && (out[j].Parent != "") })
	return out
}

func (p *pkgSet) update(eco, name string, f func(*store.GrantPackage) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.m[pkgKey(eco, name)]; e != nil {
		return f(e)
	}
	return false
}

func mergeExtras(a, b string) string {
	if b == "" {
		return a
	}
	set := map[string]bool{}
	var all []string
	for _, x := range strings.Split(a+","+b, ",") {
		if x != "" && !set[x] {
			set[x] = true
			all = append(all, x)
		}
	}
	sort.Strings(all)
	return strings.Join(all, ",")
}

// packageStates is the grant's package list as the API shows it.
func packageStates(gps []store.GrantPackage) json.RawMessage {
	st := make([]store.PackageState, 0, len(gps))
	for _, p := range gps {
		st = append(st, p.State())
	}
	b, _ := json.Marshal(st)
	return b
}

// initPackages puts the roots of a new packages grant into memory and the database.
func (m *Manager) initPackages(ctx context.Context, l *grantLive, roots []netgrant.PkgRoot) {
	l.pkgs = newPkgSet()
	for _, r := range roots {
		gp := store.GrantPackage{GrantID: l.g.ID, Ecosystem: r.Ecosystem, Name: r.Name, Version: r.Version, Extras: strings.Join(r.Extras, ",")}
		if l.pkgs.put(gp) {
			if err := m.st.AddGrantPackage(ctx, gp); err != nil {
				slog.Warn("grant package not stored", "grant", l.g.ID, "package", r.Name, "err", err)
			}
		}
	}
}

// loadPackages restores the set of a packages grant after a restart.
func (m *Manager) loadPackages(ctx context.Context, l *grantLive) {
	l.pkgs = newPkgSet()
	gps, err := m.st.GrantPackages(ctx, l.g.ID)
	if err != nil {
		slog.Warn("grant packages not loaded", "grant", l.g.ID, "err", err)
		return
	}
	for _, gp := range gps {
		l.pkgs.put(gp)
	}
}

// packageRoots reads the roots a pending packages grant was created with.
func packageRoots(g store.Grant) []netgrant.PkgRoot {
	var roots []netgrant.PkgRoot
	_ = json.Unmarshal(g.Packages, &roots)
	return roots
}

// pkgSource is the manager as the package gate's source.
type pkgSource struct{ *Manager }

// PackageSource returns the source for the package gate.
func (m *Manager) PackageSource() pkggate.Source { return pkgSource{m} }

func normVersion(v string) string { return strings.ToLower(strings.ReplaceAll(v, "_", "-")) }

// Check decides on a package request by the chat's active grants: full allows everything; a packages grant allows
// its roots, their declared dependencies and (pip) the build tools; a pinned root only in that version.
func (s pkgSource) Check(chat string, q pkggate.Query) pkggate.Verdict {
	m := s.Manager
	now := time.Now()
	var act []*grantLive
	for _, l := range m.grants.active(chat) {
		if why := l.over(now); why != "" {
			go m.EndGrant(context.Background(), chat, l.g.ID, why)
			continue
		}
		act = append(act, l)
	}
	if len(act) == 0 {
		if e, ok := m.grants.ended(chat); ok && time.Since(e.at) < 5*time.Minute {
			return pkggate.Verdict{Reason: netgrant.MsgGrantEnded(e.id, netgrant.EndWhy(e.reason)), Code: netgrant.CodeAccessEnded}
		}
		return pkggate.Verdict{Reason: netgrant.MsgOff, Code: netgrant.CodeInternetOff}
	}
	for _, l := range act {
		if l.g.Kind == string(netgrant.Full) {
			return pkggate.Verdict{Allow: true, GrantID: l.g.ID, Full: true}
		}
	}
	v := pkggate.Verdict{}
	var mismatch *pkggate.Verdict
	var first int64
	for _, l := range act {
		if l.g.Kind != string(netgrant.Packages) || l.pkgs == nil {
			continue
		}
		if first == 0 {
			first = l.g.ID
		}
		e, ok := l.pkgs.get(q.Eco, q.Name)
		if !ok {
			continue
		}
		if e.Version != "" && q.Version != "" && normVersion(e.Version) != normVersion(q.Version) {
			mv := pkggate.Verdict{GrantID: l.g.ID, Code: netgrant.CodeVersionMismatch, Reason: netgrant.MsgVersionMismatch(q.Eco, q.Name, e.Version, q.Version)}
			mismatch = &mv
			continue
		}
		if !v.Allow {
			v.Allow, v.GrantID, v.Parent = true, l.g.ID, e.Parent
			if e.Extras != "" {
				v.Extras = strings.Split(e.Extras, ",")
			}
		}
		v.Grants = append(v.Grants, l.g.ID)
	}
	if v.Allow {
		return v
	}
	if mismatch != nil {
		return *mismatch
	}
	if q.Eco == netgrant.Pip && netgrant.IsBuildTool(q.Name) && first != 0 {
		return pkggate.Verdict{Allow: true, GrantID: first, Parent: "build tool"}
	}
	return pkggate.Verdict{Code: netgrant.CodePackageNotListed, Reason: netgrant.MsgPackageNotListed(q.Eco, q.Name)}
}

// AddDeps lets the declared dependencies of a package join the grants that cover it.
func (s pkgSource) AddDeps(chat, eco, parent string, grants []int64, deps []netgrant.Dep) {
	m := s.Manager
	for _, id := range grants {
		l := m.grants.get(id)
		if l == nil || l.pkgs == nil || l.g.ChatID != chat {
			continue
		}
		changed := false
		for _, d := range deps {
			gp := store.GrantPackage{GrantID: id, Ecosystem: eco, Name: d.Name, Parent: parent, Extras: strings.Join(d.Extras, ",")}
			if l.pkgs.put(gp) {
				changed = true
				if err := m.st.AddGrantPackage(context.Background(), gp); err != nil {
					slog.Warn("grant package not stored", "grant", id, "package", d.Name, "err", err)
				}
			}
		}
		if changed {
			m.publishGrant(l)
		}
	}
}

// Downloaded notes the first file of a package that went through the gate.
func (s pkgSource) Downloaded(chat, eco, name string, grants []int64) {
	m := s.Manager
	for _, id := range grants {
		l := m.grants.get(id)
		if l == nil || l.pkgs == nil || l.g.ChatID != chat {
			continue
		}
		now := time.Now()
		if l.pkgs.update(eco, name, func(e *store.GrantPackage) bool {
			if e.DownloadedAt != nil {
				return false
			}
			e.DownloadedAt = &now
			return true
		}) {
			_ = m.st.MarkDownloaded(context.Background(), id, eco, name)
			m.publishGrant(l)
		}
	}
}

func (s pkgSource) Still(chat string, grant int64) (bool, string) {
	return webGate{s.Manager}.Still(chat, grant)
}
func (s pkgSource) Count(grant, n int64)               { webGate{s.Manager}.Count(grant, n) }
func (s pkgSource) RecordWeb(r webproxy.Request) int64 { return s.Manager.RecordWeb(r) }

// --- "installed" ---

type instState struct{ running, dirty bool }

// checkInstalled looks, after a command, whether the packages of an active packages grant are installed in the
// sandbox. It runs in the background, one check at a time per chat (a request during a check repeats it once).
func (m *Manager) checkInstalled(chatID string) {
	if chatID == "" || !m.hasPackagesGrant(chatID) {
		return
	}
	m.instMu.Lock()
	if m.inst == nil {
		m.inst = map[string]*instState{}
	}
	st := m.inst[chatID]
	if st == nil {
		st = &instState{}
		m.inst[chatID] = st
	}
	if st.running {
		st.dirty = true
		m.instMu.Unlock()
		return
	}
	st.running = true
	m.instMu.Unlock()
	go func() {
		for {
			m.runInstalledCheck(chatID)
			m.instMu.Lock()
			if !st.dirty {
				st.running = false
				m.instMu.Unlock()
				return
			}
			st.dirty = false
			m.instMu.Unlock()
		}
	}()
}

func (m *Manager) hasPackagesGrant(chatID string) bool {
	for _, l := range m.grants.active(chatID) {
		if l.g.Kind == string(netgrant.Packages) {
			return true
		}
	}
	return false
}

// installedScript prints {"pip:name": ["version", …], "npm:name": ["version", …]} for the wanted names it finds installed
// (every version found: a system package of another version must not hide the one in the sandbox's virtualenv):
// pip through importlib.metadata (the interpreter's sites) and *.dist-info below /workspace and the home directory,
// npm through node_modules/<name>/package.json below /workspace and the global prefixes.
const installedScript = `
import sys, os, re, json
want = json.loads(sys.argv[1])
out = {}
def norm(n): return re.sub(r"[-_.]+", "-", n).lower()
pw = set(want.get("pip", []))
nw = list(want.get("npm", []))
if pw:
    try:
        import importlib.metadata as md
        for d in md.distributions():
            n = norm(d.metadata["Name"] or "")
            if n in pw:
                out.setdefault("pip:" + n, []).append(d.version)
    except Exception:
        pass
budget = 60000
roots = ["/workspace", os.path.expanduser("~")]
for root in roots:
    base_depth = root.rstrip("/").count("/")
    for dp, dn, fn in os.walk(root):
        budget -= 1
        if budget < 0:
            break
        if dp.count("/") - base_depth >= 8:
            dn[:] = []
        keep = []
        for d in dn:
            if d == ".git" or d == "__pycache__":
                continue
            if d.endswith(".dist-info"):
                m = re.match(r"(.+?)-(\d[^-]*)\.dist-info$", d)
                if m and norm(m.group(1)) in pw:
                    out.setdefault("pip:" + norm(m.group(1)), []).append(m.group(2))
                continue
            if d == "node_modules":
                for n in nw:
                    pj = os.path.join(dp, d, n, "package.json")
                    if os.path.isfile(pj):
                        try:
                            out.setdefault("npm:" + n, []).append(json.load(open(pj)).get("version", ""))
                        except Exception:
                            pass
                continue
            keep.append(d)
        dn[:] = keep
for g in ["/home/agent/.local/lib/node_modules", os.path.expanduser("~/.local/lib/node_modules"), "/usr/local/lib/node_modules", "/usr/lib/node_modules"]:
    for n in nw:
        pj = os.path.join(g, n, "package.json")
        if os.path.isfile(pj):
            try:
                out.setdefault("npm:" + n, []).append(json.load(open(pj)).get("version", ""))
            except Exception:
                pass
print(json.dumps(out))
`

func (m *Manager) runInstalledCheck(chatID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	want := map[string][]string{}
	seen := map[string]bool{}
	for _, l := range m.grants.active(chatID) {
		if l.g.Kind != string(netgrant.Packages) || l.pkgs == nil {
			continue
		}
		for _, p := range l.pkgs.list() {
			k := pkgKey(p.Ecosystem, p.Name)
			if p.InstalledAt == nil && !seen[k] && len(seen) < 400 {
				seen[k] = true
				want[p.Ecosystem] = append(want[p.Ecosystem], p.Name)
			}
		}
	}
	if len(seen) == 0 {
		return
	}
	m.mu.Lock()
	lv := m.live[chatID]
	m.mu.Unlock()
	if lv == nil {
		return
	}
	arg, _ := json.Marshal(want)
	out, err := lv.slot.Worker.Exec(ctx, []string{"python3", "-I", "-c", installedScript, string(arg)}, nil)
	if err != nil {
		slog.Debug("installed check failed", "chat", chatID, "err", err)
		return
	}
	var found map[string][]string
	if json.Unmarshal(lastLine(out), &found) != nil {
		return
	}
	m.applyInstalled(ctx, chatID, found)
}

func lastLine(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return []byte(s)
}

// applyInstalled marks the found packages and ends a grant whose roots are all installed.
func (m *Manager) applyInstalled(ctx context.Context, chatID string, found map[string][]string) {
	for _, l := range m.grants.active(chatID) {
		if l.g.Kind != string(netgrant.Packages) || l.pkgs == nil {
			continue
		}
		changed := false
		for _, p := range l.pkgs.list() {
			vers := found[pkgKey(p.Ecosystem, p.Name)]
			if len(vers) == 0 || p.InstalledAt != nil {
				continue
			}
			ver := vers[len(vers)-1] // the last one found lies below the workspace or home, after the system's
			if p.Version != "" {
				ver = ""
				for _, v := range vers {
					if normVersion(p.Version) == normVersion(v) {
						ver = v
					}
				}
				if ver == "" {
					continue // other versions are there; the pinned one is not installed yet
				}
			}
			now := time.Now()
			if l.pkgs.update(p.Ecosystem, p.Name, func(e *store.GrantPackage) bool {
				e.InstalledAt, e.InstalledVersion = &now, ver
				return true
			}) {
				changed = true
				_ = m.st.MarkInstalled(ctx, l.g.ID, p.Ecosystem, p.Name, ver)
			}
		}
		if changed {
			m.publishGrant(l)
		}
		all := true
		for _, p := range l.pkgs.list() {
			if p.Parent == "" && p.InstalledAt == nil {
				all = false
			}
		}
		if all {
			m.EndGrant(ctx, chatID, l.g.ID, store.EndInstalled)
		}
	}
}

// pkgHandler builds the handler of the package gate for one slot.
func (m *Manager) pkgHandler(g *pkggate.Gate, slotID string, toolCall func() string) http.Handler {
	return g.Handler(func() string { c, _ := m.AttributeSlot(slotID); return c }, toolCall)
}

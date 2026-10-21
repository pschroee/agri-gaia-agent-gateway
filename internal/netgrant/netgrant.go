// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package netgrant holds the rules of the internet grants (issue #175), without any I/O: the kinds of access, the
// parsing and normalising of what the agent asks for, the limits, and the decision whether a connection is covered
// by the grants a chat has right now. The web proxy, the manager and the socket all ask the same code, so the card
// the user approves, the log and the enforcement cannot drift apart.
package netgrant

import (
	"fmt"
	"net"
	"strings"
)

// Kind is the kind of an internet grant. One grant has exactly one kind; a chat can hold several grants.
type Kind string

const (
	// Search: web_search and web_extract (pi's tools) and nothing else.
	Search Kind = "search"
	// Domains: https to the listed host names, exact, no wildcards.
	Domains Kind = "domains"
	// Packages: pip and npm packages through the package gate, until they are installed (stage 2).
	Packages Kind = "packages"
	// Full: the whole internet as before issue #175 (the egress network), with limits.
	Full Kind = "full"
)

// Kinds lists the kinds in the order the user is shown them (narrowest first).
var Kinds = []Kind{Search, Domains, Packages, Full}

// ParseKind returns the kind named s (exact, lower case).
func ParseKind(s string) (Kind, bool) {
	for _, k := range Kinds {
		if string(k) == s {
			return k, true
		}
	}
	return "", false
}

// KindList is "search, domains, packages, full" for messages.
func KindList() string {
	names := make([]string, len(Kinds))
	for i, k := range Kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// Origin is where a connection comes from: pi's container (web_search, web_extract), the execution sandbox
// (commands the agent runs) or the package gate (pip and npm, stage 2).
type Origin string

const (
	OriginPi   Origin = "pi"
	OriginExec Origin = "exec"
	OriginPkg  Origin = "pkg"
)

// SearxHost is the name under which pi addresses our own SearXNG service.
const SearxHost = "searxng"

// Limits of the parts of a grant that the agent may only propose.
const (
	MaxDomains = 10
	MaxGrants  = 5 // active grants per chat
	// MaxReason: a reason is a note, not an essay.
	MaxReason = 500
	// A packages grant has fixed safety limits (it ends when the packages are installed, stage 2).
	PackagesMinutes = 60
	PackagesMB      = 4000
)

// Limits are the defaults and upper bounds for time and data of search, domains and full grants.
type Limits struct {
	MinutesDefault, MinutesMax int
	MBDefault, MBMax           int
}

// DefaultLimits: 30 minutes and 500 MB by default, at most 120 minutes and 5000 MB.
func DefaultLimits() Limits {
	return Limits{MinutesDefault: 30, MinutesMax: 120, MBDefault: 500, MBMax: 5000}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MinutesDefault <= 0 {
		l.MinutesDefault = d.MinutesDefault
	}
	if l.MinutesMax <= 0 {
		l.MinutesMax = d.MinutesMax
	}
	if l.MBDefault <= 0 {
		l.MBDefault = d.MBDefault
	}
	if l.MBMax <= 0 {
		l.MBMax = d.MBMax
	}
	l.MinutesDefault = min(l.MinutesDefault, l.MinutesMax)
	l.MBDefault = min(l.MBDefault, l.MBMax)
	return l
}

// Spec is what the agent (or the user's switch) asks for.
type Spec struct {
	Kind     Kind
	Reason   string
	Domains  []string
	Packages []string // raw "pip:name[==ver]" / "npm:name[@ver]" entries (stage 2)
	Minutes  int      // proposed by the agent; 0: default
	MB       int
}

// Clamp validates the spec and returns it with normalised domains and limits inside the bounds.
func Clamp(s Spec, l Limits) (Spec, error) {
	l = l.withDefaults()
	if _, ok := ParseKind(string(s.Kind)); !ok {
		return s, fmt.Errorf("kind must be one of %s", KindList())
	}
	switch s.Kind {
	case Domains:
		d, err := ParseDomains(s.Domains)
		if err != nil {
			return s, err
		}
		s.Domains = d
	default:
		if len(s.Domains) > 0 {
			return s, fmt.Errorf("a host list belongs to kind domains, not to %s", s.Kind)
		}
	}
	if s.Kind != Packages && len(s.Packages) > 0 {
		return s, fmt.Errorf("a package list belongs to kind packages, not to %s", s.Kind)
	}
	if s.Kind == Packages {
		roots, err := ParsePackages(s.Packages)
		if err != nil {
			return s, err
		}
		s.Packages = s.Packages[:0:0]
		for _, r := range roots {
			s.Packages = append(s.Packages, r.String())
		}
		s.Minutes, s.MB = PackagesMinutes, PackagesMB
		return s, nil
	}
	s.Minutes = clampInt(s.Minutes, l.MinutesDefault, l.MinutesMax)
	s.MB = clampInt(s.MB, l.MBDefault, l.MBMax)
	return s, nil
}

func clampInt(v, def, hi int) int {
	switch {
	case v <= 0:
		return def
	case v > hi:
		return hi
	}
	return v
}

var deniedSuffixes = []string{".local", ".internal", ".lan", ".home.arpa", ".localhost"}

// ParseDomains normalises a list of exact host names: lower case, no trailing dot, no duplicates. Entries may
// contain commas (the command line passes "a.org,b.org"). Rejected: wildcards, schemes, paths and ports, IP
// literals, names without a dot, localhost and the local suffixes, anything outside ASCII (give the punycode name),
// and more than MaxDomains entries.
func ParseDomains(raw []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, entry := range raw {
		for _, h := range strings.Split(entry, ",") {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			n, err := normalizeHost(h)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", h, err)
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("give at least one host name")
	}
	if len(out) > MaxDomains {
		return nil, fmt.Errorf("at most %d host names per request", MaxDomains)
	}
	return out, nil
}

func normalizeHost(h string) (string, error) {
	if strings.ContainsAny(h, "*") {
		return "", fmt.Errorf("wildcards are not allowed, list each host name")
	}
	if strings.ContainsAny(h, "/:?#@[]\\ ") {
		return "", fmt.Errorf("give the host name only, without scheme, port, path or credentials")
	}
	for _, r := range h {
		if r > 127 {
			return "", fmt.Errorf("use the ASCII (punycode) form of the host name")
		}
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if net.ParseIP(h) != nil {
		return "", fmt.Errorf("IP addresses are not allowed, use the host name")
	}
	if !strings.Contains(h, ".") {
		return "", fmt.Errorf("a host name needs a dot (no single-label names)")
	}
	if h == "localhost" {
		return "", fmt.Errorf("localhost is not allowed")
	}
	for _, s := range deniedSuffixes {
		if strings.HasSuffix(h, s) {
			return "", fmt.Errorf("local names (%s) are not allowed", s)
		}
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return "", fmt.Errorf("not a valid host name")
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", fmt.Errorf("not a valid host name")
			}
		}
	}
	if allDigits(labels[len(labels)-1]) {
		return "", fmt.Errorf("IP addresses are not allowed, use the host name")
	}
	return h, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// NormalizeHost lower-cases a host as the proxy sees it and drops the trailing dot.
func NormalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// Active is what the decision needs of an active grant.
type Active struct {
	ID      int64
	Kind    Kind
	Domains []string
}

// Codes of refusals (web_requests.reason): short words the UI turns into its own labels; the text for the agent is
// Decision.Reason. A refusal that no grant covers (not listed, internet off, grant ended) carries no grant ID and is
// counted at the chat.
const (
	CodeNotListed        = "not_listed"
	CodeHTTPSOnly        = "https_only"
	CodeSNIMismatch      = "sni_mismatch"
	CodeIPLiteral        = "ip_literal"
	CodeSearchOnly       = "search_only"
	CodeAccessEnded      = "access_ended"
	CodeInternetOff      = "internet_off"
	CodePackageNotListed = "package_not_listed"
	CodeVersionMismatch  = "version_mismatch"
	CodeNotPublic        = "not_public"
	CodePortNotAllowed   = "port_not_allowed"
	CodeUpstream         = "upstream"
	CodeUnassigned       = "unassigned"
	CodeCacheRedirect    = "cache_redirect"
	CodeBadRequest       = "bad_request"
	CodeTooLarge         = "too_large"
)

// Decision is the verdict on one connection. GrantID names the grant that allowed it, or, for a refusal that a grant
// does cover in part (https only, search only), that grant; 0 when no grant is involved (the refusal counts at the
// chat).
type Decision struct {
	GrantID int64
	Kind    Kind
	Allow   bool
	Reason  string // the text of the refusal, starts with "blocked: "
	Code    string // the code of the refusal (Code…)
}

// Texts of refusals. They name the reason and the next step, because the agent reads them in the failing
// command's output.
const (
	MsgOnlyHTTPS    = "blocked: only https is allowed for listed domains"
	MsgSearchOnly   = "blocked: search-only access covers web_search and web_extract, not commands"
	MsgOff          = "blocked: internet access is off for this chat; ask the user with agw-internet"
	MsgNoChat       = "request not assignable to a chat"
	MsgNeedsCommand = "needs the command line"
	MsgIPLiteral    = "blocked: IP addresses are not allowed, use the host name and list it with agw-internet domains"
)

// MsgPackageNotListed: the package gate refuses a package that no grant covers.
func MsgPackageNotListed(eco, name string) string {
	return fmt.Sprintf("blocked: %s package %s is not part of the allowed packages or their dependencies; ask with agw-internet packages %s:%s \"<reason>\"", eco, name, eco, name)
}

// MsgVersionMismatch: the agent asked for exactly one version of a root package.
func MsgVersionMismatch(eco, name, want, got string) string {
	return fmt.Sprintf("blocked: %s package %s is allowed in version %s only, not %s", eco, name, want, got)
}

// MsgTooLarge: a package file does not fit into the sandbox. size < 0: the answer had no length, max was passed.
// tmpMB and homeMB are the sizes of /tmp and the home directory; grant 0 leaves out the hint to end the access.
func MsgTooLarge(eco, name, file string, size, max, tmpMB, homeMB, grant int64) string {
	has := fmt.Sprintf("has more than %d MB", max>>20)
	if size >= 0 {
		has = fmt.Sprintf("has %d MB", size>>20)
	}
	msg := fmt.Sprintf("blocked: %s package %s is too large for this sandbox: %s %s, package files may have at most %d MB "+
		"(/tmp holds %d MB, the home directory %s). Large ML libraries such as torch and torchvision are preinstalled in "+
		"notebook runs: use the skill notebook.", eco, name, file, has, max>>20, tmpMB, mbOrGB(homeMB))
	if grant > 0 {
		msg += fmt.Sprintf(" If this access is no longer needed, end it with agw-internet off %d", grant)
	}
	return msg
}

func mbOrGB(mb int64) string {
	if mb >= 1024 && mb%1024 == 0 {
		return fmt.Sprintf("%d GB", mb>>10)
	}
	return fmt.Sprintf("%d MB", mb)
}

// MsgNotListed: the host is not part of any grant.
func MsgNotListed(host string) string {
	return fmt.Sprintf("blocked: %s is not in the allowed list of this chat; ask for it with agw-internet domains %s \"<reason>\"", host, host)
}

// MsgSNIMismatch: the TLS server name differs from the host of CONNECT.
func MsgSNIMismatch(sni, host string) string {
	return fmt.Sprintf("blocked: TLS server name %s does not match %s", sni, host)
}

// MsgGrantEnded: the grant ended; why is "time limit", "data limit", "installed" or "switched off".
func MsgGrantEnded(id int64, why string) string {
	return fmt.Sprintf("blocked: grant %d ended (%s)", id, why)
}

// EndWhy turns an ended_reason into the words of MsgGrantEnded.
func EndWhy(reason string) string {
	switch reason {
	case "time":
		return "time limit"
	case "bytes":
		return "data limit"
	case "installed":
		return "installed"
	}
	return "switched off"
}

// Decide checks one connection against the chat's active grants. Precedence: full, then a domains grant that lists
// the host, then search. Commands of the sandbox (OriginExec) are never covered by a search grant.
func Decide(grants []Active, o Origin, host string, port int) Decision {
	if len(grants) == 0 {
		return Decision{Reason: MsgOff, Code: CodeInternetOff}
	}
	host = NormalizeHost(host)
	var search *Active
	var listed *Active
	for i := range grants {
		g := &grants[i]
		switch g.Kind {
		case Full:
			return Decision{GrantID: g.ID, Kind: Full, Allow: true}
		case Domains:
			for _, d := range g.Domains {
				if d == host {
					if listed == nil {
						listed = g
					}
					break
				}
			}
		case Search:
			if search == nil {
				search = g
			}
		}
	}
	if listed != nil {
		if port != 443 {
			return Decision{GrantID: listed.ID, Reason: MsgOnlyHTTPS, Code: CodeHTTPSOnly}
		}
		return Decision{GrantID: listed.ID, Kind: Domains, Allow: true}
	}
	if search != nil {
		if o == OriginExec {
			return Decision{GrantID: search.ID, Reason: MsgSearchOnly, Code: CodeSearchOnly}
		}
		if o == OriginPi {
			return Decision{GrantID: search.ID, Kind: Search, Allow: true}
		}
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return Decision{Reason: MsgIPLiteral, Code: CodeIPLiteral}
	}
	return Decision{Reason: MsgNotListed(host), Code: CodeNotListed}
}

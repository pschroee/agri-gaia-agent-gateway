// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package netgrant

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseDomains(t *testing.T) {
	good := []struct {
		in   []string
		want []string
	}{
		{[]string{"download.tensorflow.org"}, []string{"download.tensorflow.org"}},
		{[]string{"Download.TensorFlow.org."}, []string{"download.tensorflow.org"}},           // case and trailing dot
		{[]string{"a.example.org,b.example.org"}, []string{"a.example.org", "b.example.org"}}, // comma list
		{[]string{" a.example.org ", "a.example.org", "b.example.org"}, []string{"a.example.org", "b.example.org"}},
		{[]string{"xn--bcher-kva.example"}, []string{"xn--bcher-kva.example"}}, // punycode form
		{[]string{"my-host.example.org"}, []string{"my-host.example.org"}},
	}
	for _, c := range good {
		got, err := ParseDomains(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
		}
	}
	bad := map[string]string{
		"*.example.org":                  "wildcard",
		"example.*":                      "wildcard",
		"https://example.org":            "scheme",
		"example.org:443":                "port",
		"example.org/path":               "path",
		"user@example.org":               "credentials",
		"1.2.3.4":                        "IP literal",
		"::1":                            "IPv6",
		"[::1]":                          "IPv6 in brackets",
		"2130706433":                     "number, no dot",
		"127.1":                          "short IP form",
		"localhost":                      "localhost",
		"intranet":                       "no dot",
		"printer.local":                  ".local",
		"db.internal":                    ".internal",
		"nas.lan":                        ".lan",
		"x.home.arpa":                    ".home.arpa",
		"foo.localhost":                  ".localhost",
		"bücher.example":                 "non-ASCII (IDN)",
		"-bad.example.org":               "leading hyphen",
		"bad-.example.org":               "trailing hyphen",
		"a..example.org":                 "empty label",
		"under_score.example":            "underscore",
		strings.Repeat("a", 64) + ".org": "label too long",
	}
	for h, why := range bad {
		if got, err := ParseDomains([]string{h}); err == nil {
			t.Errorf("%q (%s) accepted: %q", h, why, got)
		}
	}
	if _, err := ParseDomains(nil); err == nil {
		t.Error("empty list accepted")
	}
	if _, err := ParseDomains([]string{" , "}); err == nil {
		t.Error("blank list accepted")
	}
	var many []string
	for i := 0; i < MaxDomains; i++ {
		many = append(many, fmt.Sprintf("h%d.example.org", i))
	}
	if got, err := ParseDomains(many); err != nil || len(got) != MaxDomains {
		t.Errorf("exactly %d: %d %v", MaxDomains, len(got), err)
	}
	if _, err := ParseDomains(append(many, "one-more.example.org")); err == nil {
		t.Errorf("more than %d accepted", MaxDomains)
	}
}

func TestClamp(t *testing.T) {
	l := DefaultLimits()
	got, err := Clamp(Spec{Kind: Search, Reason: "x"}, l)
	if err != nil || got.Minutes != 30 || got.MB != 500 {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	got, _ = Clamp(Spec{Kind: Full, Minutes: 45, MB: 800}, l)
	if got.Minutes != 45 || got.MB != 800 {
		t.Fatalf("proposal inside the bounds: %+v", got)
	}
	got, _ = Clamp(Spec{Kind: Full, Minutes: 100000, MB: 100000}, l)
	if got.Minutes != 120 || got.MB != 5000 {
		t.Fatalf("clamped to the bounds: %+v", got)
	}
	got, _ = Clamp(Spec{Kind: Full, Minutes: -5, MB: -1}, l)
	if got.Minutes != 30 || got.MB != 500 {
		t.Fatalf("negative takes the default: %+v", got)
	}
	// Configured limits; a default above the maximum is cut down.
	got, _ = Clamp(Spec{Kind: Search}, Limits{MinutesDefault: 500, MinutesMax: 60, MBDefault: 10, MBMax: 20})
	if got.Minutes != 60 || got.MB != 10 {
		t.Fatalf("configured: %+v", got)
	}
	// Domains are normalised, and only the domains kind takes a list.
	got, err = Clamp(Spec{Kind: Domains, Domains: []string{"A.Example.org"}}, l)
	if err != nil || got.Domains[0] != "a.example.org" {
		t.Fatalf("domains: %+v %v", got, err)
	}
	for _, bad := range []Spec{
		{Kind: Domains}, // no list
		{Kind: Domains, Domains: []string{"*.example.org"}}, // wildcard
		{Kind: Search, Domains: []string{"a.example.org"}},  // list for another kind
		{Kind: Full, Packages: []string{"pip:torch"}},       // packages for another kind
		{Kind: "everything"}, {Kind: ""},                    // unknown kind
	} {
		if _, err := Clamp(bad, l); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// Packages have fixed safety limits, whatever is proposed.
	got, _ = Clamp(Spec{Kind: Packages, Packages: []string{"pip:torch"}, Minutes: 5, MB: 5}, l)
	if got.Minutes != PackagesMinutes || got.MB != PackagesMB {
		t.Fatalf("packages: %+v", got)
	}
}

func TestParseKind(t *testing.T) {
	for _, k := range []string{"search", "domains", "packages", "full"} {
		if got, ok := ParseKind(k); !ok || string(got) != k {
			t.Errorf("%s: %q %v", k, got, ok)
		}
	}
	for _, k := range []string{"", "Search", "all", "internet"} {
		if _, ok := ParseKind(k); ok {
			t.Errorf("%q accepted", k)
		}
	}
	if KindList() != "search, domains, packages, full" {
		t.Errorf("list: %s", KindList())
	}
}

// The matrix of kind x origin x host: which grant allows or refuses what.
func TestDecideOrigins(t *testing.T) {
	search := Active{ID: 1, Kind: Search}
	domains := Active{ID: 2, Kind: Domains, Domains: []string{"download.tensorflow.org", "files.example.org"}}
	full := Active{ID: 3, Kind: Full}
	type c struct {
		name   string
		grants []Active
		o      Origin
		host   string
		port   int
		allow  bool
		id     int64
		kind   Kind
		reason string // part of the refusal
	}
	cases := []c{
		{"no grant", nil, OriginPi, "example.org", 443, false, 0, "", "internet access is off for this chat"},
		{"no grant, sandbox", nil, OriginExec, "example.org", 443, false, 0, "", "ask the user with agw-internet"},

		{"search, pi, any host", []Active{search}, OriginPi, "anything.example.org", 443, true, 1, Search, ""},
		{"search, pi, http", []Active{search}, OriginPi, "anything.example.org", 80, true, 1, Search, ""},
		{"search, pi, searx", []Active{search}, OriginPi, "searxng", 8080, true, 1, Search, ""},
		{"search, sandbox", []Active{search}, OriginExec, "anything.example.org", 443, false, 1, "", "search-only access covers web_search and web_extract, not commands"},
		{"search, sandbox, searx", []Active{search}, OriginExec, "searxng", 8080, false, 1, "", "not commands"},

		{"domains, listed, sandbox", []Active{domains}, OriginExec, "download.tensorflow.org", 443, true, 2, Domains, ""},
		{"domains, listed, pi", []Active{domains}, OriginPi, "files.example.org", 443, true, 2, Domains, ""},
		{"domains, listed, case and dot", []Active{domains}, OriginExec, "Download.TensorFlow.org.", 443, true, 2, Domains, ""},
		{"domains, listed, http", []Active{domains}, OriginExec, "download.tensorflow.org", 80, false, 2, "", "only https is allowed for listed domains"},
		{"domains, listed, other port", []Active{domains}, OriginExec, "download.tensorflow.org", 8443, false, 2, "", "only https"},
		{"domains, not listed", []Active{domains}, OriginExec, "github.com", 443, false, 0, "", `github.com is not in the allowed list of this chat; ask for it with agw-internet domains github.com "<reason>"`},
		{"domains, subdomain is not listed", []Active{domains}, OriginExec, "x.download.tensorflow.org", 443, false, 0, "", "not in the allowed list"},
		{"domains, IP literal", []Active{domains}, OriginPi, "93.184.216.34", 443, false, 0, "", "IP addresses are not allowed"},
		{"domains, searx", []Active{domains}, OriginPi, "searxng", 8080, false, 0, "", "not in the allowed list"},

		{"full, sandbox", []Active{full}, OriginExec, "anything.example.org", 443, true, 3, Full, ""},
		{"full, pi, searx", []Active{full}, OriginPi, "searxng", 8080, true, 3, Full, ""},
		{"full, port 22", []Active{full}, OriginExec, "anything.example.org", 22, true, 3, Full, ""},

		// Precedence: full over everything; a listing domains grant over search for the same host.
		{"all three, unlisted host", []Active{search, domains, full}, OriginExec, "anything.example.org", 443, true, 3, Full, ""},
		{"search + domains, listed host, http", []Active{search, domains}, OriginPi, "download.tensorflow.org", 80, false, 2, "", "only https"},
		{"search + domains, listed host, https", []Active{search, domains}, OriginPi, "download.tensorflow.org", 443, true, 2, Domains, ""},
		{"search + domains, unlisted, pi", []Active{search, domains}, OriginPi, "other.example.org", 443, true, 1, Search, ""},
		{"search + domains, unlisted, sandbox", []Active{search, domains}, OriginExec, "other.example.org", 443, false, 1, "", "not commands"},
		{"package gate, search only", []Active{search}, OriginPkg, "pypi.org", 443, false, 0, "", "not in the allowed list"},
	}
	for _, tc := range cases {
		d := Decide(tc.grants, tc.o, tc.host, tc.port)
		if d.Allow != tc.allow || d.GrantID != tc.id || d.Kind != tc.kind || !strings.Contains(d.Reason, tc.reason) {
			t.Errorf("%s: %+v, want allow=%v grant=%d kind=%q reason~%q", tc.name, d, tc.allow, tc.id, tc.kind, tc.reason)
		}
		if !tc.allow && d.Code == "" {
			t.Errorf("%s: a refusal carries a code", tc.name)
		}
		if !tc.allow && !strings.HasPrefix(d.Reason, "blocked: ") && d.Reason != MsgNoChat {
			t.Errorf("%s: a refusal starts with \"blocked: \": %q", tc.name, d.Reason)
		}
	}
}

func TestMessages(t *testing.T) {
	if got := MsgSNIMismatch("other.test", "target.test"); got != "blocked: TLS server name other.test does not match target.test" {
		t.Error(got)
	}
	for reason, why := range map[string]string{"time": "time limit", "bytes": "data limit", "installed": "installed", "user": "switched off", "agent": "switched off", "": "switched off"} {
		if got := EndWhy(reason); got != why {
			t.Errorf("EndWhy(%q) = %q, want %q", reason, got, why)
		}
	}
	if got := MsgGrantEnded(7, "data limit"); got != "blocked: grant 7 ended (data limit)" {
		t.Error(got)
	}
}

func TestMsgTooLarge(t *testing.T) {
	m := MsgTooLarge("pip", "torch", "torch-2.5.1-cp312-cp312-manylinux_2_28_x86_64.whl", 555<<20, 256<<20, 512, 1024, 8)
	for _, want := range []string{"blocked: pip package torch is too large for this sandbox", "has 555 MB", "at most 256 MB",
		"/tmp holds 512 MB", "home directory 1 GB", "skill notebook", "agw-internet off 8"} {
		if !strings.Contains(m, want) {
			t.Errorf("%q misses %q", m, want)
		}
	}
	if m := MsgTooLarge("npm", "x", "x-1.tgz", -1, 256<<20, 512, 1024, 0); !strings.Contains(m, "x-1.tgz has more than 256 MB") || strings.Contains(m, "agw-internet off") {
		t.Errorf("without length or grant: %q", m)
	}
}

// The wording of the package message must not change when the tail is shared.
func TestMsgTooLargeUnchanged(t *testing.T) {
	got := MsgTooLarge("pip", "torch", "t.whl", 555<<20, 256<<20, 512, 1024, 8)
	want := "blocked: pip package torch is too large for this sandbox: t.whl has 555 MB, package files may have at most 256 MB " +
		"(/tmp holds 512 MB, the home directory 1 GB). Large ML libraries such as torch and torchvision are preinstalled in " +
		"notebook runs: use the skill notebook. If this access is no longer needed, end it with agw-internet off 8"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestMsgDownloadTooLarge(t *testing.T) {
	m := MsgDownloadTooLarge("files.example.org", "/data/big.zip", 700<<20, 256<<20, 512, 1024, 8)
	for _, want := range []string{"blocked: the download from files.example.org is too large for this sandbox", "/data/big.zip has 700 MB",
		"at most 256 MB", "/tmp holds 512 MB", "home directory 1 GB", "skill notebook", "agw-internet off 8"} {
		if !strings.Contains(m, want) {
			t.Errorf("%q misses %q", m, want)
		}
	}
	m = MsgDownloadTooLarge("files.example.org", "", -1, 256<<20, 512, 1024, 0)
	for _, want := range []string{"the connection to files.example.org was closed after 256 MB", "at most 256 MB per connection"} {
		if !strings.Contains(m, want) {
			t.Errorf("tunnel: %q misses %q", m, want)
		}
	}
	if strings.Contains(m, "agw-internet off") {
		t.Errorf("without a grant: %q", m)
	}
	if m := MsgDownloadTooLarge("h", "/x", -1, 256<<20, 512, 1024, 0); !strings.Contains(m, "/x has more than 256 MB") {
		t.Errorf("without length: %q", m)
	}
}

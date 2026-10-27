// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Issue #229: Chromium in the sandbox must not ask Google's services in the background, and what it still tries
// must fail closed at a dead local port, not at the relay. One file in /etc/chromium.d/ reaches every starter.
func TestChromiumOfflineFile(t *testing.T) {
	s := repoFile(t, "images/agw-basis/chromium/agw-offline")
	if !strings.Contains(s, "SPDX-FileCopyrightText: 2026 Philipp Schröer") || !strings.Contains(s, "SPDX-License-Identifier: MIT") {
		t.Error("SPDX header missing")
	}
	for _, f := range []string{
		"--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-default-apps",
		"--disable-domain-reliability", "--disable-client-side-phishing-detection", "--metrics-recording-only",
		"--no-pings", "--no-first-run", "--no-default-browser-check",
	} {
		if !strings.Contains(s, f) {
			t.Errorf("switch %s missing", f)
		}
	}
	guard := `if [ "${AGW_CHROMIUM_ONLINE:-}" != 1 ]; then`
	i := strings.Index(s, guard)
	if i < 0 {
		t.Fatalf("guard %q missing", guard)
	}
	rest := s[i:]
	j := strings.Index(rest, "\nfi")
	if j < 0 || !strings.Contains(rest[:j], "--proxy-server=http://127.0.0.1:9") {
		t.Error("--proxy-server=http://127.0.0.1:9 must sit inside the AGW_CHROMIUM_ONLINE guard")
	}
	if strings.Contains(s[:i], "--proxy-server") {
		t.Error("--proxy-server outside the guard")
	}
	// The launcher expands CHROMIUM_FLAGS unquoted: no token may hold a space or a glob character.
	tok := regexp.MustCompile(`^--[a-z0-9-]+(=[^ *?\[]+)?$`)
	assign := regexp.MustCompile(`(?m)^\s*export CHROMIUM_FLAGS="\$CHROMIUM_FLAGS ([^"]*)"$`)
	ms := assign.FindAllStringSubmatch(s, -1)
	if len(ms) != 2 {
		t.Fatalf("want 2 CHROMIUM_FLAGS assignments, got %d", len(ms))
	}
	for _, m := range ms {
		for _, f := range strings.Split(m[1], " ") {
			if !tok.MatchString(f) {
				t.Errorf("token %q would be split or globbed by the launcher", f)
			}
		}
	}
}

func TestDockerfileChromiumOffline(t *testing.T) {
	s := repoFile(t, "images/agw-basis/Dockerfile")
	i := strings.Index(s, "AS exec\n")
	if i < 0 {
		t.Fatal("stage exec missing")
	}
	exec := s[i:]
	if k := strings.Index(exec, "\nFROM "); k >= 0 {
		exec = exec[:k]
	}
	inst := strings.Index(exec, "install -y --no-install-recommends chromium")
	cp := strings.Index(exec, "COPY images/agw-basis/chromium/agw-offline /etc/chromium.d/agw-offline")
	if inst < 0 || cp < inst {
		t.Error("agw-offline must be copied after Chromium is installed")
	}
	for _, w := range []string{
		"COPY images/agw-basis/chromium/netcheck.py /opt/agw/chromium/netcheck.py",
		"rm -f /etc/chromium.d/extensions /etc/chromium.d/apikeys",
	} {
		if !strings.Contains(exec, w) {
			t.Errorf("Dockerfile stage exec lacks %q", w)
		}
	}
	// plotly, kaleido and choreographer are pinned in the hashed lock (#242); kaleido's switches and MathJax host are
	// what the allow list below depends on.
	lock := repoFile(t, "images/agw-basis/python/requirements.lock")
	for _, w := range []string{"\nplotly==7.1.0 ", "\nkaleido==1.5.0 ", "\nchoreographer==1.4.0 "} {
		if !strings.Contains(lock, w) {
			t.Errorf("requirements.lock lacks %q", strings.TrimSpace(w))
		}
	}
	calls := regexp.MustCompile(`netcheck\.py[^\n]*`).FindAllString(exec, -1)
	var run []string
	for _, c := range calls {
		if strings.Contains(c, "-- ") {
			run = append(run, c)
		}
	}
	if len(run) != 4 {
		t.Fatalf("want 4 netcheck calls, got %d: %v", len(run), run)
	}
	for n, w := range []string{"-- mmdc ", "-- mermaid-check ", "-- timeout 60 chromium --headless", "write_image"} {
		if !strings.Contains(run[n], w) {
			t.Errorf("netcheck call %d lacks %q: %s", n+1, w, run[n])
		}
	}
	// Hosts that end at the dead port (H1, measured in Chromium 155): Google's domains on every start (which hosts
	// varies with load); MathJax and Debian's default search only from kaleido's page. Nothing else may be allowed.
	for n, c := range run {
		m := regexp.MustCompile(`--allow (\S+) --`).FindStringSubmatch(c)
		if m == nil {
			t.Errorf("netcheck call %d has no --allow list: %s", n+1, c)
			continue
		}
		want := ".google.com,.googleapis.com"
		if n == 3 {
			want = ".google.com,.googleapis.com,cdnjs.cloudflare.com,start.duckduckgo.com"
		}
		if m[1] != want {
			t.Errorf("netcheck call %d allows %q, want %q", n+1, m[1], want)
		}
	}
	pol := strings.Index(exec, "COPY images/agw-basis/chromium/agw-offline.json /etc/chromium/policies/managed/agw-offline.json")
	if pol < inst {
		t.Error("the Chromium policy must be copied after Chromium is installed")
	}
}

func TestChromiumPolicy(t *testing.T) {
	var p map[string]any
	if err := json.Unmarshal([]byte(repoFile(t, "images/agw-basis/chromium/agw-offline.json")), &p); err != nil {
		t.Fatalf("agw-offline.json: %v", err)
	}
	want := map[string]any{
		"ComponentUpdatesEnabled": false, "BrowserSignin": float64(0), "SyncDisabled": true,
		"MetricsReportingEnabled": false, "BrowserNetworkTimeQueriesEnabled": false,
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("policy %s = %v, want %v", k, p[k], v)
		}
	}
}

func TestNetcheckScript(t *testing.T) {
	s := repoFile(t, "images/agw-basis/chromium/netcheck.py")
	if !strings.Contains(s, "SPDX-FileCopyrightText: 2026 Philipp Schröer") || !strings.Contains(s, "SPDX-License-Identifier: MIT") {
		t.Error("SPDX header missing")
	}
	for _, w := range []string{
		"127.0.0.1", "Listener(9)", "http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY",
		"netcheck: request bypassed --proxy-server: ", "netcheck: Chromium still calls out: ", "netcheck OK: ",
		`a.startswith(".")`,
	} {
		if !strings.Contains(s, w) {
			t.Errorf("netcheck.py lacks %q", w)
		}
	}
}

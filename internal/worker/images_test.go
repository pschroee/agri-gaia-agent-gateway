// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Issue #242: everything the agent images fetch from outside must be pinned so that a rebuild gets the same bytes, and
// a package its author unpublishes (pi-searxng-suite, 2026-10-25) cannot break the build. These tests read the build
// files; they guard the lines the pins live in, a Docker build is not part of them. images/check-artifacts.py asks
// the registries whether the pinned artifacts are still there.

var sha256Ref = regexp.MustCompile(`@sha256:[0-9a-f]{64}(\s|$)`)

// instructions returns the lines of a Dockerfile without comments, continuation lines folded into one.
func instructions(t *testing.T, rel string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(repoFile(t, rel), "\\\n", " "), "\n") {
		if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, regexp.MustCompile(`\s+`).ReplaceAllString(s, " "))
		}
	}
	return out
}

var imageDockerfiles = []string{"Dockerfile", "images/agw-basis/Dockerfile"}

func TestImagesBaseImagesPinned(t *testing.T) {
	for _, rel := range imageDockerfiles {
		stages := map[string]bool{}
		for _, l := range instructions(t, rel) {
			m := regexp.MustCompile(`(?i)^FROM (\S+)(?: AS (\S+))?`).FindStringSubmatch(l)
			if m == nil {
				continue
			}
			if !stages[m[1]] && !sha256Ref.MatchString(m[1]+" ") {
				t.Errorf("%s: base image not pinned by digest: %s", rel, l)
			}
			if m[2] != "" {
				stages[m[2]] = true
			}
		}
	}
	for _, rel := range []string{"compose.yaml", "compose.hot.yaml"} {
		n := 0
		for _, l := range instructions(t, rel) {
			m := regexp.MustCompile(`^image:\s*["']?(\S+?)["']?$`).FindStringSubmatch(l)
			if m == nil || strings.HasPrefix(m[1], "agwpoc/") { // built here
				continue
			}
			n++
			if !sha256Ref.MatchString(m[1] + " ") {
				t.Errorf("%s: image not pinned by digest: %s", rel, m[1])
			}
		}
		if n == 0 {
			t.Errorf("%s: no image: line found", rel)
		}
	}
}

func TestImagesDownloadsVerified(t *testing.T) {
	d := repoFile(t, "images/agw-basis/Dockerfile")
	for _, p := range []string{"TYPST", "FD"} {
		for _, a := range []string{"AMD64", "ARM64"} {
			if !regexp.MustCompile(`(?m)^ARG ` + p + `_SHA256_` + a + `=[0-9a-f]{64}$`).MatchString(d) {
				t.Errorf("ARG %s_SHA256_%s missing or not 64 hex characters", p, a)
			}
		}
	}
	curl := regexp.MustCompile(`\bcurl\s+-`)
	n := 0
	for _, rel := range imageDockerfiles {
		for _, l := range instructions(t, rel) {
			if !curl.MatchString(l) {
				continue
			}
			n++
			if strings.Contains(l, "| tar") || !regexp.MustCompile(`curl [^|&]*-o /tmp/\S+`).MatchString(l) {
				t.Errorf("%s: curl must load into a file, not into a pipe: %s", rel, l)
			}
			if !strings.Contains(l, "sha256sum -c") {
				t.Errorf("%s: download without sha256sum -c in the same RUN: %s", rel, l)
			}
		}
	}
	if n != 2 {
		t.Errorf("found %d downloads, want 2 (typst, fd); a new one needs a checksum and a test line", n)
	}
}

type npmLock struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		Link      bool   `json:"link"`
		Resolved  string `json:"resolved"`
		Integrity string `json:"integrity"`
	} `json:"packages"`
}

func readNpmLock(t *testing.T, dir string) npmLock {
	t.Helper()
	var l npmLock
	if err := json.Unmarshal([]byte(repoFile(t, filepath.Join(dir, "package-lock.json"))), &l); err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return l
}

func TestImagesNpmLocked(t *testing.T) {
	npmCi := 0
	for _, rel := range imageDockerfiles {
		for _, l := range instructions(t, rel) {
			if strings.Contains(l, "pi install") {
				t.Errorf("%s: pi install fetches unlocked packages: %s", rel, l)
			}
			for _, m := range regexp.MustCompile(`npm install\b[^&;|]*`).FindAllString(l, -1) {
				if !regexp.MustCompile(`^npm install -g @earendil-works/pi-coding-agent@\$\{PI_VERSION\}\s*$`).MatchString(m) {
					t.Errorf("%s: npm install other than the pinned pi (it ships an npm-shrinkwrap.json): %s", rel, m)
				}
			}
			if strings.Contains(l, "npm ci") {
				npmCi++
			}
		}
	}
	if npmCi != 4 { // web, pi-packages, pi-subagents-deps, mmdc
		t.Errorf("found %d npm ci, want 4", npmCi)
	}
	d := repoFile(t, "images/agw-basis/Dockerfile")
	for _, w := range []string{"COPY images/agw-basis/pi-packages /opt/agw/pihome/npm", "COPY images/agw-basis/pi-subagents-deps /tmp/pi-subagents-deps",
		"COPY images/agw-basis/mmdc/package.json images/agw-basis/mmdc/package-lock.json /tmp/mmdc/"} {
		if !strings.Contains(d, w) {
			t.Errorf("Dockerfile lacks %q", w)
		}
	}
	if !strings.Contains(repoFile(t, "Dockerfile"), "COPY web/package.json web/package-lock.json ./") {
		t.Error("Dockerfile does not copy the lockfile of the web UI")
	}
	for _, dir := range []string{"web", "images/agw-basis/pi-packages", "images/agw-basis/mmdc", "images/agw-basis/pi-subagents-deps"} {
		l := readNpmLock(t, dir)
		if l.LockfileVersion != 3 {
			t.Errorf("%s: lockfileVersion %d, want 3", dir, l.LockfileVersion)
		}
		for k, p := range l.Packages {
			if k == "" || p.Link {
				continue
			}
			if p.Integrity == "" || !strings.HasPrefix(p.Resolved, "https://registry.npmjs.org/") {
				t.Errorf("%s: %s without integrity or not resolved from registry.npmjs.org", dir, k)
			}
		}
		if dir == "web" {
			continue
		}
		var pkg struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if err := json.Unmarshal([]byte(repoFile(t, dir+"/package.json")), &pkg); err != nil {
			t.Fatal(err)
		}
		if len(pkg.Dependencies) == 0 {
			t.Errorf("%s/package.json has no dependencies", dir)
		}
		for n, v := range pkg.Dependencies {
			if strings.HasPrefix(v, "^") || strings.HasPrefix(v, "~") || !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
				t.Errorf("%s/package.json: %s %q is not an exact version", dir, n, v)
			}
		}
	}
}

func TestImagesPipLocked(t *testing.T) {
	d := repoFile(t, "images/agw-basis/Dockerfile")
	var installs []string
	for _, l := range instructions(t, "images/agw-basis/Dockerfile") {
		if strings.Contains(l, "pip3 install") {
			installs = append(installs, l)
		}
	}
	if len(installs) != 1 {
		t.Fatalf("found %d pip3 install, want exactly 1 (the hashed lock): %v", len(installs), installs)
	}
	for _, w := range []string{"--require-hashes", "--no-deps", "--only-binary :all:", "-r /tmp/requirements.lock"} {
		if !strings.Contains(installs[0], w) {
			t.Errorf("pip3 install lacks %q: %s", w, installs[0])
		}
	}
	if strings.Contains(d, "constraints") {
		t.Error("the Dockerfile still mentions a constraints file")
	}
	lock := repoFile(t, "images/agw-basis/python/requirements.lock")
	pins := map[string]bool{}
	lines := strings.Split(lock, "\n")
	req := regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)==(\S+) \\$`)
	for i, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "    --hash=") {
			continue
		}
		m := req.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("requirements.lock line %d is not `name==version \\`: %q", i+1, l)
			continue
		}
		if i+1 >= len(lines) || !regexp.MustCompile(`^    --hash=sha256:[0-9a-f]{64}( \\)?$`).MatchString(lines[i+1]) {
			t.Errorf("requirements.lock: %s has no hash", m[1])
		}
		pins[strings.ToLower(m[1])+"=="+m[2]] = true
		switch strings.ToLower(m[1]) {
		case "pip", "setuptools", "wheel":
			t.Errorf("requirements.lock pins %s, which the image's Debian packages own", m[1])
		}
	}
	md := regexp.MustCompile(`ARG MARKITDOWN_VERSION=(\S+)`).FindStringSubmatch(d)
	if md == nil {
		t.Fatal("MARKITDOWN_VERSION not found")
	}
	for _, w := range []string{"onnxruntime==1.30.0", "plotly==7.1.0", "kaleido==1.5.0", "choreographer==1.4.0",
		"markitdown==" + md[1], "nbformat==5.10.4", "nbclient==0.10.4", "ipykernel==7.2.0"} {
		if !pins[w] {
			t.Errorf("requirements.lock lacks %s", w)
		}
	}
	if strings.Contains(lock, "onnxruntime==1.20") {
		t.Error("onnxruntime 1.20 in the lock; the image runs 1.30.0")
	}
}

func TestImagesVendoredSearxng(t *testing.T) {
	var pkg struct{ Name, Version string }
	if err := json.Unmarshal([]byte(repoFile(t, "third_party/searxng-suite/package.json")), &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "@blazer2k/searxng-suite" || pkg.Version != "0.2.3" {
		t.Errorf("vendored package is %s %s, want @blazer2k/searxng-suite 0.2.3", pkg.Name, pkg.Version)
	}
	for _, f := range []string{"src/index.ts", "VENDORED.md", "LICENSE"} {
		if _, err := os.Stat(filepath.Join("..", "..", "third_party", "searxng-suite", f)); err != nil {
			t.Error(err)
		}
	}
	if !strings.Contains(repoFile(t, "REUSE.toml"), "third_party/searxng-suite/**") {
		t.Error("REUSE.toml does not cover third_party/searxng-suite")
	}
	for _, w := range []string{"COPY third_party/searxng-suite /opt/agw/pihome/npm/node_modules/@blazer2k/searxng-suite", "COPY third_party/pi-intercom /opt/agw/pihome/npm/node_modules/pi-intercom"} {
		if !strings.Contains(repoFile(t, "images/agw-basis/Dockerfile"), w) {
			t.Errorf("Dockerfile lacks %q", w)
		}
	}
	p := repoFile(t, "images/agw-basis/pi-packages/package.json")
	for _, n := range []string{"searxng-suite", "pi-intercom"} {
		if strings.Contains(p, n+"\"") {
			t.Errorf("pi-packages/package.json names %s, which comes from third_party", n)
		}
	}
}

// The inventory of the pre-rollout check lists every kind and every entry of the locks, without network access.
func TestCheckArtifactsList(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	cmd := exec.Command(py, "-I", filepath.Join("..", "..", "images", "check-artifacts.py"), "--list")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("check-artifacts.py --list: %v", err)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		have[l] = true
	}
	if len(have) < 100 {
		t.Errorf("--list printed only %d entries", len(have))
	}
	for _, kind := range []string{"image", "npm", "pypi", "go", "apt", "download", "typst"} {
		if !strings.Contains(string(out), "\n"+kind+"\t") && !strings.HasPrefix(string(out), kind+"\t") {
			t.Errorf("--list names no %s", kind)
		}
	}
	for _, line := range strings.Split(repoFile(t, "images/agw-basis/python/requirements.lock"), "\n") {
		if m := regexp.MustCompile(`^(\S+)==(\S+) \\$`).FindStringSubmatch(line); m != nil && !have["pypi\t"+m[1]+"@"+m[2]] {
			t.Errorf("--list lacks pypi %s@%s", m[1], m[2])
		}
	}
	for _, dir := range []string{"images/agw-basis/pi-packages", "images/agw-basis/mmdc", "images/agw-basis/pi-subagents-deps"} {
		for k, p := range readNpmLock(t, dir).Packages {
			if k == "" || p.Link {
				continue
			}
			name := k[strings.LastIndex(k, "node_modules/")+len("node_modules/"):]
			ver := lockVersion(t, dir, k)
			if !have["npm\t"+name+"@"+ver] {
				t.Errorf("--list lacks npm %s@%s", name, ver)
			}
		}
	}
	if !have["npm\t@earendil-works/pi-coding-agent@0.87.1"] {
		t.Error("--list lacks the globally installed pi")
	}
}

func lockVersion(t *testing.T, dir, key string) string {
	t.Helper()
	var l struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal([]byte(repoFile(t, dir+"/package-lock.json")), &l); err != nil {
		t.Fatal(err)
	}
	return l.Packages[key].Version
}

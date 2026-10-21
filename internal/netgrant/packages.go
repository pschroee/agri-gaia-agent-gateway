// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package netgrant

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Packages grants (issue #175, stage 2): the agent names root packages (pip:name[==ver], npm:name[@ver]); the
// package gate lets through those roots, the dependencies that allowed packages declare themselves, and for pip a
// fixed list of build tools. The rules here are pure, the gate and the manager apply them.

// Ecosystems of the package gate.
const (
	Pip = "pip"
	Npm = "npm"
)

// MaxPackages is the number of root packages in one request.
const MaxPackages = 20

// PkgRoot is a package the agent asked for. Version is an exact pin ("" = any version, the card says so).
type PkgRoot struct {
	Ecosystem string   `json:"ecosystem"`
	Name      string   `json:"name"` // pip: normalised (PEP 503), npm: as given
	Version   string   `json:"version,omitempty"`
	Extras    []string `json:"extras,omitempty"` // pip only, normalised, sorted
}

// String is the canonical form shown on the card and accepted again by ParsePackages.
func (r PkgRoot) String() string {
	s := r.Ecosystem + ":" + r.Name
	if len(r.Extras) > 0 {
		s += "[" + strings.Join(r.Extras, ",") + "]"
	}
	if r.Version != "" {
		if r.Ecosystem == Pip {
			s += "==" + r.Version
		} else {
			s += "@" + r.Version
		}
	}
	return s
}

// BuildTools are the pip packages that build isolation loads without anyone declaring them (issue #175). They are
// allowed whenever a packages grant is active.
var BuildTools = []string{"setuptools", "wheel", "pip", "flit-core", "hatchling", "hatch-vcs", "poetry-core",
	"setuptools-scm", "scikit-build-core", "meson-python", "maturin", "cython", "pdm-backend"}

// IsBuildTool reports whether the (normalised) pip name is a build tool.
func IsBuildTool(name string) bool {
	for _, b := range BuildTools {
		if b == name {
			return true
		}
	}
	return false
}

var (
	normRe     = regexp.MustCompile(`[-_.]+`)
	pipNameRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	npmNameRe  = regexp.MustCompile(`^(@[a-z0-9~][a-z0-9._~-]*/)?[a-z0-9~][a-z0-9._~-]*$`)
	versionRe  = regexp.MustCompile(`^[0-9][A-Za-z0-9.+!_-]*$`)
	extraNmeRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
)

// NormalizePip normalises a pip project name as PEP 503 does: runs of - _ . become one hyphen, lower case.
func NormalizePip(name string) string {
	return strings.ToLower(normRe.ReplaceAllString(strings.TrimSpace(name), "-"))
}

// splitList splits a list at commas that stand outside square brackets (extras may contain commas).
func splitList(entry string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range entry {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, entry[start:i])
				start = i + 1
			}
		}
	}
	return append(out, entry[start:])
}

// ParsePackages reads the root packages of a request: entries "pip:name[extra,…][==version]" and
// "npm:name[@version]" (name may be scoped, @scope/name), separated by commas or given as several entries. pip
// names are normalised, duplicates merged (the first pin wins), at most MaxPackages. Version ranges are not
// accepted: a pin is exact or absent.
func ParsePackages(raw []string) ([]PkgRoot, error) {
	var out []PkgRoot
	seen := map[string]bool{}
	for _, entry := range raw {
		for _, e := range splitList(entry) {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			r, err := parsePackage(e)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", e, err)
			}
			k := r.Ecosystem + ":" + r.Name
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("give at least one package, e.g. pip:name or npm:name")
	}
	if len(out) > MaxPackages {
		return nil, fmt.Errorf("at most %d packages per request", MaxPackages)
	}
	return out, nil
}

func parsePackage(e string) (PkgRoot, error) {
	eco, rest, ok := strings.Cut(e, ":")
	if !ok {
		return PkgRoot{}, fmt.Errorf("start with the ecosystem: pip:%s or npm:%s", e, e)
	}
	var r PkgRoot
	switch strings.ToLower(eco) {
	case Pip:
		r.Ecosystem = Pip
		name, ver, pinned := strings.Cut(rest, "==")
		if pinned && strings.TrimSpace(ver) == "" {
			return r, fmt.Errorf("a version must follow ==")
		}
		if strings.ContainsAny(ver, "<>=~!,; ") || strings.Contains(name, "==") {
			return r, fmt.Errorf("a version must be exact: name==1.2.3")
		}
		var extras []string
		if i := strings.Index(name, "["); i >= 0 {
			if !strings.HasSuffix(name, "]") {
				return r, fmt.Errorf("extras must be written name[extra1,extra2]")
			}
			for _, x := range strings.Split(name[i+1:len(name)-1], ",") {
				x = strings.TrimSpace(x)
				if x == "" {
					continue
				}
				if !extraNmeRe.MatchString(x) {
					return r, fmt.Errorf("not a valid extra: %q", x)
				}
				extras = append(extras, NormalizePip(x))
			}
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if strings.ContainsAny(name, "<>~!") {
			return r, fmt.Errorf("a version must be exact: name==1.2.3 (no ranges)")
		}
		if strings.ContainsAny(name, ";/ ") || !pipNameRe.MatchString(name) {
			return r, fmt.Errorf("not a valid pip package name")
		}
		r.Name = NormalizePip(name)
		r.Extras = uniqSorted(extras)
		ver = strings.TrimSpace(ver)
		if ver != "" && !versionRe.MatchString(ver) {
			return r, fmt.Errorf("not a valid version")
		}
		r.Version = ver
	case Npm:
		r.Ecosystem = Npm
		name, ver := rest, ""
		if i := strings.LastIndex(rest, "@"); i > 0 {
			name, ver = rest[:i], rest[i+1:]
		}
		name = strings.TrimSpace(name)
		if len(name) > 214 || !npmNameRe.MatchString(strings.ToLower(name)) || strings.ContainsAny(name, " ") {
			return r, fmt.Errorf("not a valid npm package name")
		}
		if ver != "" && (strings.ContainsAny(ver, "^~<>=*| ") || !versionRe.MatchString(ver)) {
			return r, fmt.Errorf("a version must be exact: name@1.2.3 (no ranges or tags)")
		}
		r.Name, r.Version = name, ver
	default:
		return r, fmt.Errorf("unknown ecosystem %q, use pip or npm", eco)
	}
	return r, nil
}

func uniqSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// Dep is a dependency a package declares: the (normalised) name and the extras it asks of it.
type Dep struct {
	Name   string
	Extras []string
}

var extraMarkerRe = regexp.MustCompile(`extra\s*==\s*["']([^"']+)["']`)

// RequiresDistDeps reads the Requires-Dist lines of core metadata (PEP 658 .metadata, METADATA of a wheel,
// PKG-INFO of an sdist). Entries with an "extra ==" marker count only if one of the extras is among the
// requested ones; other markers (platform, Python version) are not evaluated: every such dependency counts.
func RequiresDistDeps(metadata []byte, extras []string) []Dep {
	want := map[string]bool{}
	for _, x := range extras {
		want[NormalizePip(x)] = true
	}
	var out []Dep
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(metadata))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var cur string
	flush := func() {
		if cur == "" {
			return
		}
		line := cur
		cur = ""
		spec, marker, _ := strings.Cut(line, ";")
		if ms := extraMarkerRe.FindAllStringSubmatch(marker, -1); len(ms) > 0 {
			ok := false
			for _, m := range ms {
				if want[NormalizePip(m[1])] {
					ok = true
				}
			}
			if !ok {
				return
			}
		}
		d, ok := parseRequirement(spec)
		if !ok {
			return
		}
		if i, dup := seen[d.Name]; dup {
			out[i].Extras = uniqSorted(append(out[i].Extras, d.Extras...))
			return
		}
		seen[d.Name] = len(out)
		out = append(out, d)
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			break // the body (the long description) follows the first empty line
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if cur != "" {
				cur += " " + strings.TrimSpace(line)
			}
			continue
		}
		flush()
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Requires-Dist") {
			cur = strings.TrimSpace(v)
		}
	}
	flush()
	return out
}

// RequiresDist is RequiresDistDeps reduced to the names.
func RequiresDist(metadata []byte, extras []string) []string {
	var out []string
	for _, d := range RequiresDistDeps(metadata, extras) {
		out = append(out, d.Name)
	}
	return out
}

var reqRe = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*(?:\[([^\]]*)\])?`)

func parseRequirement(spec string) (Dep, bool) {
	m := reqRe.FindStringSubmatch(spec)
	if m == nil {
		return Dep{}, false
	}
	d := Dep{Name: NormalizePip(m[1])}
	for _, x := range strings.Split(m[2], ",") {
		if x = strings.TrimSpace(x); x != "" {
			d.Extras = append(d.Extras, NormalizePip(x))
		}
	}
	d.Extras = uniqSorted(d.Extras)
	return d, true
}

// NpmDeps lists the dependency names that any version in an npm packument declares (dependencies,
// optionalDependencies, peerDependencies), sorted. Every version counts: which one npm picks is not known when the
// packument is served.
func NpmDeps(packument []byte) []string {
	var p struct {
		Versions map[string]struct {
			Dependencies         map[string]json.RawMessage `json:"dependencies"`
			OptionalDependencies map[string]json.RawMessage `json:"optionalDependencies"`
			PeerDependencies     map[string]json.RawMessage `json:"peerDependencies"`
		} `json:"versions"`
	}
	if json.Unmarshal(packument, &p) != nil {
		return nil
	}
	set := map[string]bool{}
	for _, v := range p.Versions {
		for _, m := range []map[string]json.RawMessage{v.Dependencies, v.OptionalDependencies, v.PeerDependencies} {
			for n := range m {
				if npmNameRe.MatchString(strings.ToLower(n)) {
					set[n] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

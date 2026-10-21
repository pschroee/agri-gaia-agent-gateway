// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package pkggate

import (
	"net/url"
	"regexp"
	"strings"

	"agw/internal/netgrant"
)

// Kind of a request at the gate.
type Kind string

const (
	KindIndex     Kind = "index"     // pip: the file list of a project; npm: the packument
	KindMetadata  Kind = "metadata"  // pip: <file>.metadata (PEP 658)
	KindFile      Kind = "file"      // pip: a wheel or an sdist
	KindTarball   Kind = "tarball"   // npm: a package tarball
	KindPackument Kind = "packument" // npm: the packument of a package
)

// Route is what a request path at the gate means.
type Route struct {
	Eco     string // netgrant.Pip or netgrant.Npm
	Kind    Kind
	Name    string // pip: normalised project name, npm: package name (scoped: @scope/name)
	File    string // file name for files, metadata and tarballs
	Version string // version taken from the file name (files and tarballs)
}

var (
	pipSdistExts = []string{".tar.gz", ".zip", ".tar.bz2", ".tar.xz", ".tgz"}
)

// ParseRoute reads the path of a request at the gate: /index/<project>/[<file>] (pip) or /npm/<name>[/-/<file>]
// (npm). ok is false for anything else (the root listing, npm's /-/ endpoints such as audit).
func ParseRoute(path string) (Route, bool) {
	switch {
	case strings.HasPrefix(path, "/index/"):
		return parsePip(strings.TrimPrefix(path, "/index/"))
	case strings.HasPrefix(path, "/npm/"):
		return parseNpm(strings.TrimPrefix(path, "/npm/"))
	}
	return Route{}, false
}

func parsePip(rest string) (Route, bool) {
	segs := strings.Split(rest, "/")
	if len(segs) == 0 || segs[0] == "" || strings.ContainsAny(segs[0], " \\") || strings.Contains(segs[0], "..") {
		return Route{}, false
	}
	name := netgrant.NormalizePip(segs[0])
	r := Route{Eco: netgrant.Pip, Name: name}
	switch {
	case len(segs) == 1 || (len(segs) == 2 && segs[1] == ""):
		r.Kind = KindIndex
		return r, true
	case len(segs) == 2:
		file := segs[1]
		if file == "" || strings.ContainsAny(file, " \\") || strings.HasPrefix(file, ".") {
			return Route{}, false
		}
		r.File = file
		base := file
		if strings.HasSuffix(file, ".metadata") {
			r.Kind = KindMetadata
			base = strings.TrimSuffix(file, ".metadata")
		} else {
			r.Kind = KindFile
		}
		dist, ver, ok := PipFileInfo(base)
		if !ok || netgrant.NormalizePip(dist) != name {
			return Route{}, false
		}
		r.Version = ver
		return r, true
	}
	return Route{}, false
}

// PipFileInfo splits the name of a wheel or an sdist into distribution and version.
func PipFileInfo(file string) (dist, version string, ok bool) {
	if strings.HasSuffix(file, ".whl") {
		parts := strings.Split(strings.TrimSuffix(file, ".whl"), "-")
		if len(parts) < 5 {
			return "", "", false
		}
		return parts[0], parts[1], true
	}
	for _, e := range pipSdistExts {
		if strings.HasSuffix(file, e) {
			base := strings.TrimSuffix(file, e)
			i := strings.LastIndex(base, "-")
			if i <= 0 || i == len(base)-1 {
				return "", "", false
			}
			return base[:i], base[i+1:], true
		}
	}
	return "", "", false
}

var scopedRe = regexp.MustCompile(`^@[^/]+$`)

func parseNpm(rest string) (Route, bool) {
	if rest == "" || strings.HasPrefix(rest, "-/") || strings.Contains(rest, "..") {
		return Route{}, false
	}
	// npm sends a scoped name as @scope%2fname; the path arrives decoded, but accept both forms.
	if u, err := url.PathUnescape(rest); err == nil {
		rest = u
	}
	segs := strings.Split(rest, "/")
	name := segs[0]
	if scopedRe.MatchString(name) && len(segs) > 1 {
		name += "/" + segs[1]
		segs = segs[1:]
	}
	if name == "" || strings.ContainsAny(name, " \\") {
		return Route{}, false
	}
	r := Route{Eco: netgrant.Npm, Name: name}
	switch {
	case len(segs) == 1:
		r.Kind = KindPackument
		return r, true
	case len(segs) == 3 && segs[1] == "-" && strings.HasSuffix(segs[2], ".tgz"):
		r.Kind, r.File = KindTarball, segs[2]
		base := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			base = name[i+1:]
		}
		pre := base + "-"
		if !strings.HasPrefix(segs[2], pre) {
			return Route{}, false
		}
		r.Version = strings.TrimSuffix(strings.TrimPrefix(segs[2], pre), ".tgz")
		return r, true
	}
	return Route{}, false
}

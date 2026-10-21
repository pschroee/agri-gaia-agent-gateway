// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package pkggate is the package gate of the internet grants (issue #175, stage 2): the only way pip and npm in the
// execution sandbox reach a package index. Requests arrive over the slot's socket (the relay in the sandbox forwards
// /index/… and /npm/… there); the gate decides on the package name (and, for a pinned root, the version) BEFORE it
// asks the package cache, so a refused package never reaches the cache or the registry behind it. Packages that a
// request allows are scanned for the dependencies they declare, and those join the allowed set before the client
// can ask for them.
package pkggate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agw/internal/netgrant"
	"agw/internal/webproxy"
)

// Query asks whether a package may be fetched. For pip Name is the normalised project name; Version is the version
// of the requested file (empty for an index or packument request).
type Query struct {
	Eco, Name, Version string
}

// Verdict answers a Query.
type Verdict struct {
	Allow bool
	// GrantID: the grant that allowed the request (a full grant, or the first packages grant that covers the
	// package); for a refusal the grant involved (version_mismatch) or 0.
	GrantID int64
	// Grants: the packages grants that cover the package; dependencies found in its metadata join all of them.
	Grants []int64
	// Full: a full grant allowed it, dependencies are not tracked.
	Full bool
	// Parent: why the package is allowed: empty when the agent asked for it, the declaring package, or "build tool".
	Parent string
	// Extras the package was asked with (pip), needed to read its Requires-Dist.
	Extras []string
	Reason string
	Code   string
}

// Source is the manager as the package gate sees it.
type Source interface {
	Check(chat string, q Query) Verdict
	// AddDeps lets the dependencies of a package join the given packages grants; parent names the package.
	AddDeps(chat, eco, parent string, grants []int64, deps []netgrant.Dep)
	// Downloaded notes that the first file of the package went through the gate.
	Downloaded(chat, eco, name string, grants []int64)
	Still(chat string, grant int64) (ok bool, why string)
	Count(grant, n int64)
	RecordWeb(r webproxy.Request) int64
}

// Gate serves /index/… and /npm/….
type Gate struct {
	Pip, Npm *url.URL // the package caches (proxpi, Verdaccio), reached over the package network
	Source   Source
	// PublicBase is how the sandbox addresses the gate; the tarball links of a packument are rewritten to it.
	PublicBase string
	// Client fetches from the caches and follows a redirect to the registries. Nil: a plain client without
	// automatic redirects.
	Client *http.Client
	// TmpDir holds a wheel while it is scanned ("" : the system's temporary directory).
	TmpDir string

	once sync.Once
	def  *http.Client
}

// Hosts a cache may redirect to (https only). Anything else is an error.
var redirectHosts = map[string]bool{"files.pythonhosted.org": true, "pypi.org": true, "registry.npmjs.org": true}

const (
	maxMetadata  = 4 << 20
	maxPackument = 64 << 20
	holdBack     = 64 << 10
	recheckEvery = time.Second
)

// Handler returns the handler for one slot: chat names the slot's chat ("" : unassigned), toolCall the tool call a
// command belongs to (empty: unknown).
func (g *Gate) Handler(chat func() string, toolCall func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := chat()
		tc := ""
		if toolCall != nil {
			tc = toolCall()
		}
		g.serve(w, r, c, tc)
	})
}

func (g *Gate) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	// No compression and no proxy from the environment: the gate reads the bodies, and the caches sit next door.
	g.once.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DisableCompression, tr.Proxy = true, nil
		g.def = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	})
	return g.def
}

func (g *Gate) serve(w http.ResponseWriter, r *http.Request, chat, toolCall string) {
	start := time.Now()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rt, ok := ParseRoute(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if chat == "" {
		http.Error(w, netgrant.MsgNoChat, http.StatusForbidden)
		return
	}
	host := "pypi.org"
	base := g.Pip
	if rt.Eco == netgrant.Npm {
		host, base = "registry.npmjs.org", g.Npm
	}
	rec := webproxy.Request{ChatID: chat, Method: r.Method, Host: host, Port: 443, Path: pathOf(rt), Origin: string(netgrant.OriginPkg),
		ToolCallID: toolCall, StartedAt: start, Package: rt.Name, Version: rt.Version}
	isIndex := rt.Kind == KindIndex || rt.Kind == KindMetadata || rt.Kind == KindPackument
	if isIndex {
		rec.Service = "index"
	}
	v := g.Source.Check(chat, Query{Eco: rt.Eco, Name: rt.Name, Version: rt.Version})
	rec.GrantID, rec.Parent = v.GrantID, v.Parent
	deny := func(code int, why, reason string) {
		rec.Status, rec.Denied, rec.Reason, rec.DurationMs = code, why, reason, time.Since(start).Milliseconds()
		rec.Aggregate = false
		g.Source.RecordWeb(rec)
		slog.Warn("package gate: refused", "chat", chat, "eco", rt.Eco, "package", rt.Name, "reason", reason)
		http.Error(w, why, code)
	}
	if !v.Allow {
		deny(http.StatusForbidden, v.Reason, v.Code)
		return
	}
	if ok, why := g.Source.Still(chat, v.GrantID); !ok {
		rec.GrantID = 0
		deny(http.StatusForbidden, netgrant.MsgGrantEnded(v.GrantID, why), netgrant.CodeAccessEnded)
		return
	}
	if base == nil {
		deny(http.StatusServiceUnavailable, "package cache not configured", netgrant.CodeUpstream)
		return
	}
	resp, err := g.fetch(r.Context(), base, r)
	if err != nil {
		code := netgrant.CodeUpstream
		if errors.Is(err, errRedirect) {
			code = netgrant.CodeCacheRedirect
			deny(http.StatusBadGateway, "package cache redirected elsewhere", code)
			return
		}
		deny(http.StatusBadGateway, "package cache not reachable", code)
		return
	}
	defer resp.Body.Close()
	counted := &countWriter{w: w, count: func(n int64) { g.Source.Count(v.GrantID, n) }}
	finish := func(status int) {
		rec.Status, rec.BytesDown, rec.DurationMs = status, counted.n, time.Since(start).Milliseconds()
		rec.Aggregate = isIndex
		g.Source.RecordWeb(rec)
	}
	if resp.StatusCode != http.StatusOK || r.Method == http.MethodHead {
		copyHeader(w, resp, true)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(counted, resp.Body)
		finish(resp.StatusCode)
		return
	}
	still := func() error {
		if ok, why := g.Source.Still(chat, v.GrantID); !ok {
			return fmt.Errorf("grant ended (%s)", why)
		}
		return nil
	}
	switch rt.Kind {
	case KindMetadata:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadata))
		if err != nil {
			deny(http.StatusBadGateway, "package cache not reachable", netgrant.CodeUpstream)
			return
		}
		g.addDeps(chat, rt, v, netgrant.RequiresDistDeps(body, v.Extras))
		copyHeader(w, resp, false)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = counted.Write(body)
		finish(http.StatusOK)
	case KindPackument:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxPackument+1))
		if err != nil || len(body) > maxPackument {
			deny(http.StatusBadGateway, "package index answer too large or unreadable", netgrant.CodeUpstream)
			return
		}
		var deps []netgrant.Dep
		for _, n := range netgrant.NpmDeps(body) {
			deps = append(deps, netgrant.Dep{Name: n})
		}
		g.addDeps(chat, rt, v, deps)
		if nb, err := RewritePackument(body, rt.Name, g.PublicBase); err == nil {
			body = nb
		}
		copyHeader(w, resp, false)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = counted.Write(body)
		finish(http.StatusOK)
	case KindIndex:
		copyHeader(w, resp, true)
		w.WriteHeader(http.StatusOK)
		_ = copyChecked(counted, resp.Body, still)
		finish(http.StatusOK)
	case KindFile, KindTarball:
		g.streamFile(w, resp, counted, rt, v, chat, still, finish, deny)
	}
}

// streamFile passes a wheel, an sdist or a tarball on. The last holdBack bytes stay back until the file has been
// scanned for the dependencies it declares, so that the client cannot ask for them before they are allowed.
func (g *Gate) streamFile(w http.ResponseWriter, resp *http.Response, counted *countWriter, rt Route, v Verdict, chat string,
	still func() error, finish func(int), deny func(int, string, string)) {
	tmp, err := os.CreateTemp(g.TmpDir, "pkggate-*")
	if err != nil {
		deny(http.StatusInternalServerError, "no temporary file", netgrant.CodeUpstream)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	copyHeader(w, resp, true)
	w.WriteHeader(http.StatusOK)
	tw := &tailWriter{w: counted, keep: holdBack}
	if err := copyChecked(io.MultiWriter(tmp, tw), resp.Body, still); err != nil {
		// Cut the answer short: the client sees a broken download, not a complete file.
		finish(http.StatusOK)
		return
	}
	if rt.Eco == netgrant.Pip && !v.Full {
		if meta := scanPipFile(tmp.Name(), rt.File); meta != nil {
			g.addDeps(chat, rt, v, netgrant.RequiresDistDeps(meta, v.Extras))
		}
	}
	g.Source.Downloaded(chat, rt.Eco, rt.Name, v.Grants)
	_ = tw.Flush()
	finish(http.StatusOK)
}

func (g *Gate) addDeps(chat string, rt Route, v Verdict, deps []netgrant.Dep) {
	if v.Full || len(v.Grants) == 0 || len(deps) == 0 {
		return
	}
	g.Source.AddDeps(chat, rt.Eco, rt.Name, v.Grants, deps)
}

func pathOf(rt Route) string {
	if rt.File != "" {
		return rt.File
	}
	return "/" + rt.Name
}

var errRedirect = errors.New("redirect to a host that is not allowed")

// fetch asks the cache and follows at most three redirects, and only to the registries' file hosts over https.
func (g *Gate) fetch(ctx context.Context, base *url.URL, in *http.Request) (*http.Response, error) {
	u := *base
	if strings.HasPrefix(in.URL.Path, "/npm/") {
		u.Path = strings.TrimSuffix(base.Path, "/") + "/" + strings.TrimPrefix(in.URL.Path, "/npm/")
	} else {
		u.Path = strings.TrimSuffix(base.Path, "/") + in.URL.Path
	}
	u.RawPath = ""
	u.RawQuery = in.URL.RawQuery
	cur := &u
	for hop := 0; hop < 4; hop++ {
		req, err := http.NewRequestWithContext(ctx, in.Method, cur.String(), nil)
		if err != nil {
			return nil, err
		}
		// Accept passes on (proxpi answers 406 without it); Accept-Encoding does not: the gate reads the body.
		if a := in.Header.Get("Accept"); a != "" {
			req.Header.Set("Accept", a)
		}
		req.Header.Set("User-Agent", "agw-package-gate")
		resp, err := g.client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || resp.StatusCode == http.StatusNotModified {
			return resp, nil
		}
		loc, err := resp.Location()
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		sameCache := loc.Host == base.Host
		if !sameCache && !(loc.Scheme == "https" && redirectHosts[strings.ToLower(loc.Hostname())]) {
			return nil, errRedirect
		}
		cur = loc
	}
	return nil, errRedirect
}

func copyHeader(w http.ResponseWriter, resp *http.Response, length bool) {
	for _, h := range []string{"Content-Type", "Cache-Control", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if length && resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
}

// copyChecked copies and asks check about every second; an error stops the copy.
func copyChecked(dst io.Writer, src io.Reader, check func() error) error {
	buf := make([]byte, 64<<10)
	last := time.Now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if time.Since(last) >= recheckEvery {
			last = time.Now()
			if cerr := check(); cerr != nil {
				return cerr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type countWriter struct {
	w     io.Writer
	n     int64
	count func(int64)
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	if n > 0 && c.count != nil {
		c.count(int64(n))
	}
	return n, err
}

// tailWriter passes everything on except the last keep bytes, which Flush releases.
type tailWriter struct {
	w    io.Writer
	keep int
	buf  []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.keep; over > 0 {
		if _, err := t.w.Write(t.buf[:over]); err != nil {
			return 0, err
		}
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailWriter) Flush() error {
	_, err := t.w.Write(t.buf)
	t.buf = nil
	return err
}

var wheelMetaRe = regexp.MustCompile(`^[^/]+\.dist-info/METADATA$`)
var sdistInfoRe = regexp.MustCompile(`^[^/]+/PKG-INFO$`)

// scanPipFile reads the core metadata out of a downloaded wheel (METADATA) or sdist (PKG-INFO); nil if there is none.
func scanPipFile(path, file string) []byte {
	if strings.HasSuffix(file, ".whl") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil
		}
		defer zr.Close()
		for _, f := range zr.File {
			if wheelMetaRe.MatchString(f.Name) {
				rc, err := f.Open()
				if err != nil {
					return nil
				}
				defer rc.Close()
				b, _ := io.ReadAll(io.LimitReader(rc, maxMetadata))
				return b
			}
		}
		return nil
	}
	if strings.HasSuffix(file, ".tar.gz") || strings.HasSuffix(file, ".tgz") {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				return nil
			}
			if sdistInfoRe.MatchString(strings.TrimPrefix(h.Name, "./")) {
				b, _ := io.ReadAll(io.LimitReader(tr, maxMetadata))
				return b
			}
		}
	}
	return nil
}

// RewritePackument points the dist.tarball of every version at the gate (publicBase + /npm/<name>/-/<file>): the
// cache builds these links from the Host header of its request, which is not an address the sandbox can use.
func RewritePackument(body []byte, name, publicBase string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	vraw, ok := top["versions"]
	if !ok {
		return body, nil
	}
	var versions map[string]json.RawMessage
	if err := json.Unmarshal(vraw, &versions); err != nil {
		return nil, err
	}
	for ver, raw := range versions {
		var v map[string]json.RawMessage
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		var dist map[string]json.RawMessage
		if json.Unmarshal(v["dist"], &dist) != nil {
			continue
		}
		var tb string
		if json.Unmarshal(dist["tarball"], &tb) != nil || tb == "" {
			continue
		}
		file := tb[strings.LastIndex(tb, "/")+1:]
		nb, _ := json.Marshal(strings.TrimSuffix(publicBase, "/") + "/npm/" + name + "/-/" + file)
		dist["tarball"] = nb
		v["dist"], _ = json.Marshal(dist)
		versions[ver], _ = json.Marshal(v)
	}
	top["versions"], _ = json.Marshal(versions)
	return json.Marshal(top)
}

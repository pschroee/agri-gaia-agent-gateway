// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package pkggate

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agw/internal/netgrant"
	"agw/internal/webproxy"
)

type fakeSource struct {
	mu         sync.Mutex
	allow      map[string]Verdict // "eco:name" -> verdict
	deps       map[string][]string
	downloaded []string
	recs       []webproxy.Request
	counted    int64
	still      bool
}

func newSource() *fakeSource {
	return &fakeSource{allow: map[string]Verdict{}, deps: map[string][]string{}, still: true}
}

func (s *fakeSource) Check(_ string, q Query) Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.allow[q.Eco+":"+q.Name]; ok {
		return v
	}
	return Verdict{Code: netgrant.CodePackageNotListed, Reason: netgrant.MsgPackageNotListed(q.Eco, q.Name)}
}

func (s *fakeSource) AddDeps(_, eco, parent string, grants []int64, deps []netgrant.Dep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range deps {
		s.deps[parent] = append(s.deps[parent], d.Name)
		s.allow[eco+":"+d.Name] = Verdict{Allow: true, GrantID: grants[0], Grants: grants, Parent: parent}
	}
}

func (s *fakeSource) Downloaded(_, eco, name string, _ []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloaded = append(s.downloaded, eco+":"+name)
}

func (s *fakeSource) Still(string, int64) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.still {
		return false, "switched off"
	}
	return true, ""
}

func (s *fakeSource) Count(_, n int64) { atomic.AddInt64(&s.counted, n) }

func (s *fakeSource) RecordWeb(r webproxy.Request) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r)
	return int64(len(s.recs))
}

func (s *fakeSource) last() webproxy.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs[len(s.recs)-1]
}

func newGate(t *testing.T, src Source, cache http.Handler) (*Gate, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(cache)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &Gate{Pip: u, Npm: u, Source: src, PublicBase: "http://127.0.0.1:3128", TmpDir: t.TempDir()}, srv
}

func get(t *testing.T, g *Gate, path string) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	g.Handler(func() string { return "chat-1" }, func() string { return "call-1" }).ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// A refused package never reaches the cache: the fake cache sees no request at all.
func TestPkgGateDeniesBeforeUpstream(t *testing.T) {
	var hits int32
	src := newSource()
	g, _ := newGate(t, src, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	res, body := get(t, g, "/index/evilpkg/")
	if res.StatusCode != 403 || !strings.Contains(string(body), "pip package evilpkg is not part of the allowed packages") {
		t.Fatalf("answer: %d %q", res.StatusCode, body)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("the cache was asked")
	}
	if r := src.last(); r.Reason != netgrant.CodePackageNotListed || r.Origin != "pkg" || r.ToolCallID != "call-1" || r.Package != "evilpkg" || r.GrantID != 0 {
		t.Fatalf("log: %+v", r)
	}
	// A grant that ended between the decision and the request.
	src.allow["pip:ok"] = Verdict{Allow: true, GrantID: 4, Grants: []int64{4}}
	src.still = false
	if res, _ := get(t, g, "/index/ok/"); res.StatusCode != 403 || src.last().Reason != netgrant.CodeAccessEnded {
		t.Fatalf("ended grant: %d %+v", res.StatusCode, src.last())
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("the cache was asked for an ended grant")
	}
}

const torchMetadata = "Metadata-Version: 2.1\nName: torch\nVersion: 2.5.1\nRequires-Dist: filelock\nRequires-Dist: sympy (==1.13.1)\nRequires-Dist: opt-einsum; extra == 'opt'\n\nbody\n"

// The dependencies a PEP 658 metadata file declares are allowed before the client gets the answer; the index calls
// are one collapsed row, Accept goes through and Accept-Encoding does not.
func TestPkgGateAddsDependencyFromMetadata(t *testing.T) {
	var accept, enc atomic.Value
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 3, Grants: []int64{3}}
	g, _ := newGate(t, src, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept.Store(r.Header.Get("Accept"))
		enc.Store(r.Header.Get("Accept-Encoding"))
		if r.URL.Path == "/index/torch/torch-2.5.1-py3-none-any.whl.metadata" {
			io.WriteString(w, torchMetadata)
			return
		}
		io.WriteString(w, "<html></html>")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/index/torch/torch-2.5.1-py3-none-any.whl.metadata", nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	req.Header.Set("Accept-Encoding", "gzip")
	g.Handler(func() string { return "chat-1" }, nil).ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != torchMetadata {
		t.Fatalf("answer: %d %q", rec.Code, rec.Body.String())
	}
	if accept.Load() != "application/vnd.pypi.simple.v1+json" || enc.Load() != "" {
		t.Fatalf("headers to the cache: %v %v", accept.Load(), enc.Load())
	}
	if got := strings.Join(src.deps["torch"], ","); got != "filelock,sympy" {
		t.Fatalf("dependencies: %q", got)
	}
	if v := src.Check("chat-1", Query{Eco: "pip", Name: "sympy"}); !v.Allow || v.Parent != "torch" {
		t.Fatalf("sympy: %+v", v)
	}
	if r := src.last(); !r.Aggregate || r.Service != "index" || r.Host != "pypi.org" || r.GrantID != 3 {
		t.Fatalf("log of a metadata call: %+v", r)
	}
	if src.counted != int64(len(torchMetadata)) {
		t.Fatalf("counted %d bytes", src.counted)
	}
}

func wheelWith(t *testing.T, metadata string, padding int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("torch-2.5.1.dist-info/METADATA")
	io.WriteString(w, metadata)
	w, _ = zw.Create("torch/big.bin")
	w.Write(bytes.Repeat([]byte{7}, padding))
	zw.Close()
	return buf.Bytes()
}

// A wheel is scanned for its dependencies before the client has received its last bytes, and arrives complete.
func TestPkgGateScansWheelAndHoldsTail(t *testing.T) {
	wheel := wheelWith(t, torchMetadata, 300<<10)
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 3, Grants: []int64{3}}
	g, _ := newGate(t, src, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(wheel) }))
	res, body := get(t, g, "/index/torch/torch-2.5.1-py3-none-any.whl")
	if res.StatusCode != 200 || !bytes.Equal(body, wheel) {
		t.Fatalf("answer: %d, %d of %d bytes", res.StatusCode, len(body), len(wheel))
	}
	if got := strings.Join(src.deps["torch"], ","); got != "filelock,sympy" {
		t.Fatalf("dependencies from the wheel: %q", got)
	}
	if len(src.downloaded) != 1 || src.downloaded[0] != "pip:torch" {
		t.Fatalf("downloaded: %v", src.downloaded)
	}
	if r := src.last(); r.Aggregate || r.Package != "torch" || r.Version != "2.5.1" || r.BytesDown != int64(len(wheel)) || r.Path != "torch-2.5.1-py3-none-any.whl" {
		t.Fatalf("a package file is logged on its own: %+v", r)
	}
}

func TestTailWriterHoldsBack(t *testing.T) {
	var out bytes.Buffer
	tw := &tailWriter{w: &out, keep: 10}
	tw.Write([]byte("0123456"))
	if out.Len() != 0 {
		t.Fatalf("early output: %q", out.String())
	}
	tw.Write([]byte("789abcdef"))
	if out.String() != "012345" {
		t.Fatalf("passed on: %q", out.String())
	}
	tw.Flush()
	if out.String() != "0123456789abcdef" {
		t.Fatalf("after flush: %q", out.String())
	}
}

// A pinned root is refused in another version (the source answers; the gate passes the verdict on), and the pip
// project of a file must match the path.
func TestPkgGateVersionMismatch(t *testing.T) {
	src := newSource()
	src.allow["pip:timm"] = Verdict{GrantID: 2, Code: netgrant.CodeVersionMismatch, Reason: netgrant.MsgVersionMismatch("pip", "timm", "1.0.11", "1.0.12")}
	g, _ := newGate(t, src, http.NotFoundHandler())
	res, body := get(t, g, "/index/timm/timm-1.0.12-py3-none-any.whl")
	if res.StatusCode != 403 || !strings.Contains(string(body), "allowed in version 1.0.11 only") {
		t.Fatalf("answer: %d %q", res.StatusCode, body)
	}
	if r := src.last(); r.Reason != netgrant.CodeVersionMismatch || r.GrantID != 2 {
		t.Fatalf("log: %+v", r)
	}
	if res, _ := get(t, g, "/index/timm/other-1.0-py3-none-any.whl"); res.StatusCode != 404 {
		t.Fatalf("file of another project: %d", res.StatusCode)
	}
}

// The packument's tarball links point at the relay, the dependencies of all versions join the allowed set, and
// npm's audit endpoint is not served.
func TestPkgGateRewritesTarball(t *testing.T) {
	packument := `{"name":"left-pad","versions":{"1.3.0":{"dependencies":{"a":"1"},"dist":{"tarball":"http://localhost:4873/left-pad/-/left-pad-1.3.0.tgz","shasum":"x"}},"1.2.0":{"dist":{"tarball":"http://localhost:4873/left-pad/-/left-pad-1.2.0.tgz"}}},"dist-tags":{"latest":"1.3.0"}}`
	src := newSource()
	src.allow["npm:left-pad"] = Verdict{Allow: true, GrantID: 5, Grants: []int64{5}}
	g, _ := newGate(t, src, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, packument)
	}))
	res, body := get(t, g, "/npm/left-pad")
	if res.StatusCode != 200 {
		t.Fatalf("answer: %d %q", res.StatusCode, body)
	}
	var p struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
				Shasum  string `json:"shasum"`
			} `json:"dist"`
		} `json:"versions"`
		Tags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Versions["1.3.0"].Dist.Tarball != "http://127.0.0.1:3128/npm/left-pad/-/left-pad-1.3.0.tgz" || p.Versions["1.3.0"].Dist.Shasum != "x" || p.Tags["latest"] != "1.3.0" {
		t.Fatalf("rewritten packument: %s", body)
	}
	if strings.Join(src.deps["left-pad"], ",") != "a" {
		t.Fatalf("npm dependencies: %v", src.deps)
	}
	if res, _ := get(t, g, "/npm/-/npm/v1/security/advisories/bulk"); res.StatusCode != 404 {
		t.Fatalf("audit: %d", res.StatusCode)
	}
}

// A cache that redirects somewhere else is not followed.
func TestPkgGateFollowsPyPIRedirectOnly(t *testing.T) {
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 3, Grants: []int64{3}}
	g, _ := newGate(t, src, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.org/torch.whl", http.StatusFound)
	}))
	res, body := get(t, g, "/index/torch/torch-2.5.1-py3-none-any.whl")
	if res.StatusCode != 502 || !strings.Contains(string(body), "redirected elsewhere") || src.last().Reason != netgrant.CodeCacheRedirect {
		t.Fatalf("answer: %d %q %+v", res.StatusCode, body, src.last())
	}
	// A redirect inside the cache itself is followed.
	g2, _ := newGate(t, src, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index/torch/x.whl" {
			io.WriteString(w, "ok")
			return
		}
		http.Redirect(w, r, "/index/torch/x.whl", http.StatusFound)
	}))
	if res, body := get(t, g2, "/index/torch/"); res.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("same-cache redirect: %d %q", res.StatusCode, body)
	}
}

func TestRewritePackumentKeepsOtherFields(t *testing.T) {
	out, err := RewritePackument([]byte(`{"name":"@s/x","readme":"hi","versions":{"1.0.0":{"name":"@s/x","dist":{"tarball":"http://h:1/@s/x/-/x-1.0.0.tgz","integrity":"sha512-a"}}}}`), "@s/x", "http://127.0.0.1:3128/")
	if err != nil || !strings.Contains(string(out), `"http://127.0.0.1:3128/npm/@s/x/-/x-1.0.0.tgz"`) || !strings.Contains(string(out), `"integrity":"sha512-a"`) || !strings.Contains(string(out), `"readme":"hi"`) {
		t.Fatalf("%s %v", out, err)
	}
}

// --- Issue #183: package files that do not fit into the sandbox ---

// bigWheel serves a file of n bytes with a Content-Length.
func bigCache(n int, chunked bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !chunked {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		if r.Method == http.MethodHead {
			return
		}
		buf := bytes.Repeat([]byte("x"), 1024)
		for sent := 0; sent < n; sent += len(buf) {
			w.Write(buf[:min(len(buf), n-sent)])
			if chunked {
				w.(http.Flusher).Flush()
			}
		}
	})
}

func limitedGate(t *testing.T, src *fakeSource, cache http.Handler) *Gate {
	g, _ := newGate(t, src, cache)
	g.MaxFile, g.TmpMB, g.HomeMB = 1024, 512, 1024
	return g
}

func TestPkgGateRefusesTooLargeByLength(t *testing.T) {
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 8, Grants: []int64{8}}
	g := limitedGate(t, src, bigCache(2048, false))
	res, body := get(t, g, "/index/torch/torch-2.5.1-py3-none-any.whl")
	if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "torch-2.5.1-py3-none-any.whl") || !strings.Contains(string(body), "notebook") {
		t.Fatalf("answer: %d %q", res.StatusCode, body)
	}
	if r := src.last(); r.Reason != netgrant.CodeTooLarge || r.Denied == "" || r.BytesDown != 0 || r.Package != "torch" {
		t.Fatalf("log: %+v", r)
	}
	if len(src.downloaded) != 0 || len(src.deps) != 0 {
		t.Fatalf("downloaded %v, deps %v", src.downloaded, src.deps)
	}
}

func TestPkgGateTooLargeWithoutLength(t *testing.T) {
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 8, Grants: []int64{8}}
	g := limitedGate(t, src, bigCache(4096, true))
	res, body := get(t, g, "/index/torch/torch-2.5.1-py3-none-any.whl")
	if res.StatusCode != http.StatusOK || len(body) >= 4096 {
		t.Fatalf("the answer must be cut off: %d, %d bytes", res.StatusCode, len(body))
	}
	if r := src.last(); r.Reason != netgrant.CodeTooLarge || !strings.Contains(r.Denied, "more than") {
		t.Fatalf("log: %+v", r)
	}
	if len(src.downloaded) != 0 {
		t.Fatalf("downloaded: %v", src.downloaded)
	}
}

// A file of exactly MaxFile bytes goes through.
func TestPkgGateAllowsAtLimit(t *testing.T) {
	src := newSource()
	src.allow["pip:small"] = Verdict{Allow: true, GrantID: 8, Grants: []int64{8}}
	g := limitedGate(t, src, bigCache(1024, false))
	res, body := get(t, g, "/index/small/small-1.0-py3-none-any.whl")
	if res.StatusCode != 200 || len(body) != 1024 || src.last().Reason != "" {
		t.Fatalf("answer: %d, %d bytes, %+v", res.StatusCode, len(body), src.last())
	}
	// the same without a length
	g = limitedGate(t, src, bigCache(1024, true))
	if res, body := get(t, g, "/index/small/small-1.0-py3-none-any.whl"); res.StatusCode != 200 || len(body) != 1024 || src.last().Reason != "" {
		t.Fatalf("chunked: %d, %d bytes, %+v", res.StatusCode, len(body), src.last())
	}
}

func TestPkgGateTooLargeNpmTarball(t *testing.T) {
	src := newSource()
	src.allow["npm:left-pad"] = Verdict{Allow: true, GrantID: 5, Grants: []int64{5}}
	g := limitedGate(t, src, bigCache(2048, false))
	res, body := get(t, g, "/npm/left-pad/-/left-pad-1.3.0.tgz")
	if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "npm package left-pad") || src.last().Reason != netgrant.CodeTooLarge {
		t.Fatalf("answer: %d %q %+v", res.StatusCode, body, src.last())
	}
}

func TestPkgGateTooLargeFullGrant(t *testing.T) {
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 9, Grants: []int64{9}, Full: true}
	g := limitedGate(t, src, bigCache(2048, false))
	if res, _ := get(t, g, "/index/torch/torch-2.5.1-py3-none-any.whl"); res.StatusCode != http.StatusRequestEntityTooLarge || src.last().Reason != netgrant.CodeTooLarge {
		t.Fatalf("answer: %d %+v", res.StatusCode, src.last())
	}
}

// Index pages and metadata are not package files: they stay allowed above the limit.
func TestPkgGateIndexNotLimited(t *testing.T) {
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 8, Grants: []int64{8}}
	g := limitedGate(t, src, bigCache(4096, false))
	res, body := get(t, g, "/index/torch/")
	if res.StatusCode != 200 || len(body) != 4096 {
		t.Fatalf("index: %d, %d bytes", res.StatusCode, len(body))
	}
}

func TestPkgGateHeadTooLarge(t *testing.T) {
	src := newSource()
	src.allow["pip:torch"] = Verdict{Allow: true, GrantID: 8, Grants: []int64{8}}
	g := limitedGate(t, src, bigCache(2048, false))
	rec := httptest.NewRecorder()
	g.Handler(func() string { return "chat-1" }, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/index/torch/torch-2.5.1-py3-none-any.whl", nil))
	if rec.Code != http.StatusRequestEntityTooLarge || src.last().Reason != netgrant.CodeTooLarge {
		t.Fatalf("head: %d %+v", rec.Code, src.last())
	}
}

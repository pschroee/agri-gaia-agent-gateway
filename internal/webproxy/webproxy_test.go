package webproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/netgrant"
)

// gate is a fake manager: attribution, a set of active grants decided by netgrant.Decide, a byte counter per
// grant and the log.
type gate struct {
	mu       sync.Mutex
	known    bool
	grants   []netgrant.Active
	ended    string // non-empty: Still reports the grant over for this reason
	maxBytes int64  // 0: no limit
	counted  int64
	recs     []Request
}

func full() []netgrant.Active { return []netgrant.Active{{ID: 1, Kind: netgrant.Full}} }
func domains(hosts ...string) []netgrant.Active {
	return []netgrant.Active{{ID: 1, Kind: netgrant.Domains, Domains: hosts}}
}
func search() []netgrant.Active { return []netgrant.Active{{ID: 1, Kind: netgrant.Search}} }

func (g *gate) Attribute(string) (string, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.known {
		return "", ""
	}
	return "chat-1", "p-1"
}

func (g *gate) Decide(_ string, o netgrant.Origin, host string, port int) netgrant.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return netgrant.Decide(g.grants, o, host, port)
}

func (g *gate) Still(_ string, grant int64) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended != "" {
		return false, g.ended
	}
	if g.maxBytes > 0 && g.counted >= g.maxBytes {
		return false, "data limit"
	}
	return true, ""
}

func (g *gate) Count(_, n int64) {
	g.mu.Lock()
	g.counted += n
	g.mu.Unlock()
}

func (g *gate) setGrants(a []netgrant.Active) {
	g.mu.Lock()
	g.grants = a
	g.mu.Unlock()
}

func (g *gate) RecordWeb(r Request) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recs = append(g.recs, r)
	return int64(len(g.recs))
}

func (g *gate) FinishWeb(id int64, r Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if id > 0 {
		g.recs[id-1] = r
	}
}

func (g *gate) last() Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.recs[len(g.recs)-1]
}

func portOf(t *testing.T, raw string) int {
	u, _ := url.Parse(raw)
	n, _ := strconv.Atoi(u.Port())
	return n
}

// setup: proxy in front of an HTTP and an HTTPS target on 127.0.0.1, which counts as public in the
// tests; "internal.test" points to 10.0.0.5, "target.test" and "other.test" to the target.
func setup(t *testing.T, g *gate, opts ...func(*Proxy)) (client *http.Client, target, tlsTarget *httptest.Server, searx *httptest.Server) {
	t.Helper()
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from "+r.Host+r.URL.Path)
	}))
	tlsTarget = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	searx = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[],"q":"`+r.URL.Query().Get("q")+`"}`)
	}))
	t.Cleanup(func() { target.Close(); tlsTarget.Close(); searx.Close() })
	su, _ := url.Parse(searx.URL)
	p := &Proxy{Gate: g, Searx: su, HTTPPort: portOf(t, target.URL), HTTPSPort: portOf(t, tlsTarget.URL),
		Allow: func(ip net.IP) bool { return ip.IsLoopback() },
		Lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			switch host {
			case "target.test", "other.test":
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			case "internal.test":
				return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}}
	for _, o := range opts {
		o(p)
	}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	pu, _ := url.Parse(ps.URL)
	client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	return
}

func proxyHost(c *http.Client) string {
	u, _ := c.Transport.(*http.Transport).Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "x"}})
	return u.Host
}

func TestGate(t *testing.T) {
	g := &gate{}
	c, target, _, _ := setup(t, g)
	u := "http://target.test:" + strconv.Itoa(portOf(t, target.URL)) + "/x"
	if resp, err := c.Get(u); err != nil || resp.StatusCode != 403 {
		t.Fatalf("unknown source: %v %v", resp, err)
	}
	if len(g.recs) != 0 {
		t.Fatal("unknown source logged")
	}
	g.known = true
	resp, err := c.Get(u)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("no grant: %v %v", resp, err)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "internet access is off") || g.last().Denied == "" {
		t.Fatalf("reason: %q %+v", b, g.last())
	}
}

func TestForwardAndBlock(t *testing.T) {
	g := &gate{known: true, grants: full()}
	c, target, tlsTarget, _ := setup(t, g)
	port := strconv.Itoa(portOf(t, target.URL))
	resp, err := c.Get("http://target.test:" + port + "/page")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("http: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello from target.test:"+port+"/page" {
		t.Fatalf("response: %q", b)
	}
	if r := g.last(); r.Host != "target.test" || r.Status != 200 || r.BytesDown == 0 || r.Path != "/page" || r.Denied != "" || r.GrantID != 1 || r.Origin != "pi" {
		t.Fatalf("log: %+v", r)
	}
	if g.counted == 0 {
		t.Fatal("bytes not counted on the grant")
	}
	// HTTPS through CONNECT
	resp, err = c.Get("https://target.test:" + strconv.Itoa(portOf(t, tlsTarget.URL)) + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("https: %v %v", resp, err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "secure" {
		t.Fatalf("https response: %q", b)
	}
	c.CloseIdleConnections()
	for _, bad := range []string{
		"http://internal.test:" + port + "/",     // internal address
		"http://10.0.0.7:" + port + "/",          // internal address directly
		"http://target.test:" + port + "1/",      // other port
		"http://doesnotexist.test:" + port + "/", // not resolvable
	} {
		if resp, err := c.Get(bad); err != nil || resp.StatusCode != 403 {
			t.Errorf("%s: %v %v", bad, resp, err)
		}
	}
}

// With NODE_USE_ENV_PROXY Node also tunnels HTTP through CONNECT: to SearXNG and to port 80.
func TestConnectForHTTP(t *testing.T) {
	g := &gate{known: true, grants: full()}
	c, target, _, _ := setup(t, g)
	for _, tc := range []struct{ host, want string }{
		{"searxng:8080", `"q":"tunnel"`},
		{"target.test:" + strconv.Itoa(portOf(t, target.URL)), "hello from"},
	} {
		conn, err := net.Dial("tcp", proxyHost(c))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", tc.host, tc.host)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: CONNECT %v %v", tc.host, resp, err)
		}
		fmt.Fprintf(conn, "GET /search?q=tunnel HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", tc.host)
		body, _ := io.ReadAll(br)
		conn.Close()
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: %q", tc.host, body)
		}
	}
}

func TestSearxng(t *testing.T) {
	g := &gate{known: true, grants: full()}
	c, _, _, _ := setup(t, g)
	resp, err := c.Get("http://searxng:8080/search?q=pig&format=json")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("searxng: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"q":"pig"`) {
		t.Fatalf("response: %q", b)
	}
	// A search grant covers the search service too.
	g.setGrants(search())
	if resp, _ := c.Get("http://searxng:8080/search?q=x"); resp.StatusCode != 200 {
		t.Fatalf("search grant: %d", resp.StatusCode)
	}
	// A domains grant does not cover web_search, and without a grant there is no search either.
	g.setGrants(domains("target.test"))
	if resp, _ := c.Get("http://searxng:8080/search?q=x"); resp.StatusCode != 403 {
		t.Fatalf("search with a domains grant: %d", resp.StatusCode)
	}
	g.setGrants(nil)
	if resp, _ := c.Get("http://searxng:8080/search?q=x"); resp.StatusCode != 403 {
		t.Fatalf("search without internet: %d", resp.StatusCode)
	}
}

func TestPublic(t *testing.T) {
	p := &Proxy{Blocked: mustNets("203.0.113.0/24")}
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"10.1.2.3": false, "172.20.0.1": false, "192.168.1.1": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:10.0.0.1": false,
		"203.0.113.9": false,
	} {
		if got := p.public(net.ParseIP(ip)); got != want {
			t.Errorf("%s: public = %v", ip, got)
		}
	}
}

// connect opens a tunnel through the proxy to host:port.
func connect(t *testing.T, c *http.Client, hostport string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyHost(c))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", hostport, err)
	}
	return conn, br, resp
}

// An open tunnel closes as soon as its grant ends, and the log says which grant and why.
func TestGrantEndClosesTunnel(t *testing.T) {
	old := RecheckEvery
	RecheckEvery = 50 * time.Millisecond
	defer func() { RecheckEvery = old }()
	g := &gate{known: true, grants: full()}
	c, target, _, _ := setup(t, g)
	conn, br, resp := connect(t, c, "target.test:"+strconv.Itoa(portOf(t, target.URL)))
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v", resp)
	}
	g.mu.Lock()
	g.ended = "switched off"
	g.mu.Unlock()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("tunnel not closed: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(g.last().Denied, "grant 1 ended (switched off)") {
		if time.Now().After(deadline) {
			t.Fatalf("log: %+v", g.last())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The data limit of a grant closes a tunnel in the middle of a transfer: the bytes of both directions count.
func TestProxyByteLimitClosesTunnel(t *testing.T) {
	old := RecheckEvery
	RecheckEvery = 300 * time.Millisecond // long enough for the response to pass before the tunnel closes
	defer func() { RecheckEvery = old }()
	g := &gate{known: true, grants: full(), maxBytes: 100}
	c, target, _, _ := setup(t, g)
	hp := "target.test:" + strconv.Itoa(portOf(t, target.URL))
	conn, br, resp := connect(t, c, hp)
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v", resp)
	}
	fmt.Fprintf(conn, "GET /%s HTTP/1.1\r\nHost: %s\r\n\r\n", strings.Repeat("a", 200), hp) // keep-alive: the tunnel stays open
	r, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("tunnel not closed at the data limit: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(g.last().Denied, "data limit") {
		if time.Now().After(deadline) {
			t.Fatalf("log: %+v", g.last())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A new connection is refused as well, with the reason.
	_, _, resp2 := connect(t, c, hp)
	if resp2.StatusCode != 403 {
		t.Fatalf("after the limit: %d", resp2.StatusCode)
	}
}

// tlsThrough opens a tunnel to the TLS target and starts a handshake with the given server name.
func tlsThrough(t *testing.T, c *http.Client, tlsTarget *httptest.Server, host, serverName string) (*tls.Conn, error) {
	t.Helper()
	conn, br, resp := connect(t, c, host+":"+strconv.Itoa(portOf(t, tlsTarget.URL)))
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v", resp)
	}
	_ = br
	tc := tls.Client(conn, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	return tc, tc.Handshake()
}

// A domains grant lets a tunnel through only if the TLS server name is the host of CONNECT (SNI = CONNECT host).
func TestProxyDomainsSNIMismatch(t *testing.T) {
	g := &gate{known: true, grants: domains("target.test")}
	c, _, tlsTarget, _ := setup(t, g)

	// The matching name: the handshake works and the request goes through.
	tc, err := tlsThrough(t, c, tlsTarget, "target.test", "target.test")
	if err != nil {
		t.Fatalf("matching SNI: %v", err)
	}
	fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: target.test\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(tc)
	if !strings.Contains(string(body), "secure") {
		t.Fatalf("body: %q", body)
	}
	if r := g.last(); r.Denied != "" || r.GrantID != 1 {
		t.Fatalf("log of the allowed tunnel: %+v", r)
	}
	// Case and a trailing dot do not matter.
	if _, err := tlsThrough(t, c, tlsTarget, "target.test", "TARGET.test."); err != nil {
		t.Fatalf("SNI case: %v", err)
	}

	// Another name: the tunnel is closed, the row is denied and names both.
	if _, err := tlsThrough(t, c, tlsTarget, "target.test", "other.test"); err == nil {
		t.Fatal("handshake with a foreign server name succeeded")
	}
	if r := g.last(); !strings.Contains(r.Denied, "TLS server name other.test does not match target.test") || r.Status != 403 {
		t.Fatalf("log: %+v", r)
	}
	// No name at all (an IP address as server name sends none): refused.
	if _, err := tlsThrough(t, c, tlsTarget, "target.test", ""); err == nil {
		t.Fatal("handshake without a server name succeeded")
	}
	if r := g.last(); !strings.Contains(r.Denied, "no TLS server name") {
		t.Fatalf("log: %+v", r)
	}
}

// A grant with the full kind does not look at the server name.
func TestProxyFullIgnoresSNI(t *testing.T) {
	g := &gate{known: true, grants: full()}
	c, _, tlsTarget, _ := setup(t, g)
	if _, err := tlsThrough(t, c, tlsTarget, "target.test", "other.test"); err != nil {
		t.Fatalf("full: %v", err)
	}
}

// A domains grant is https only; a host that is not listed is refused with the way to ask for it.
func TestProxyDomainsHTTPRefused(t *testing.T) {
	g := &gate{known: true, grants: domains("target.test")}
	c, target, _, _ := setup(t, g)
	port := strconv.Itoa(portOf(t, target.URL))
	resp, err := c.Get("http://target.test:" + port + "/")
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("http to a listed host: %v %v", resp, err)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "only https is allowed for listed domains") {
		t.Fatalf("reason: %q", b)
	}
	// CONNECT to port 80 of the listed host is plain http as well.
	_, _, r := connect(t, c, "target.test:"+port)
	if r.StatusCode != 403 {
		t.Fatalf("CONNECT to port 80: %d", r.StatusCode)
	}
	// Not listed.
	resp, err = c.Get("http://other.test:" + port + "/")
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("not listed: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `other.test is not in the allowed list of this chat; ask for it with agw-internet domains other.test`) {
		t.Fatalf("reason: %q", b)
	}
	if r := g.last(); r.GrantID != 0 || r.Denied == "" || r.Reason != netgrant.CodeNotListed {
		t.Fatalf("a refusal no grant covers has no grant and a code: %+v", r)
	}
}

// Commands of the sandbox (origin exec) are not covered by a search grant; pi's tools are.
func TestProxyOriginExecSearchRefused(t *testing.T) {
	g := &gate{known: true, grants: search()}
	c, target, _, _ := setup(t, g, func(p *Proxy) { p.Origin = netgrant.OriginExec })
	resp, err := c.Get("http://target.test:" + strconv.Itoa(portOf(t, target.URL)) + "/")
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("exec with a search grant: %v %v", resp, err)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "search-only access covers web_search and web_extract, not commands") {
		t.Fatalf("reason: %q", b)
	}
	if r := g.last(); r.Origin != "exec" {
		t.Fatalf("origin: %+v", r)
	}
}

// A redirect is passed on, not followed: the proxy returns the 302 and never contacts the target of it.
func TestProxyDoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer redirecting.Close()
	g := &gate{known: true, grants: full()}
	// The redirecting server plays the plain-http target: its port is the proxy's "port 80".
	c, _, _, _ := setup(t, g, func(p *Proxy) { p.HTTPPort = portOf(t, redirecting.URL) })
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get("http://target.test:" + strconv.Itoa(portOf(t, redirecting.URL)) + "/")
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect: %v %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatal("the proxy followed the redirect")
	}
}

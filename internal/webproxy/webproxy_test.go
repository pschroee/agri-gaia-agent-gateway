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
)

type gate struct {
	mu       sync.Mutex
	internet bool
	known    bool
	recs     []Request
}

func (g *gate) WebAccess(string) (string, string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.known {
		return "", "", false
	}
	return "chat-1", "p-1", g.internet
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

// setup: Proxy vor einem HTTP- und einem HTTPS-Ziel auf 127.0.0.1, das in den Tests als öffentlich
// gilt; „intern.test“ zeigt auf 10.0.0.5, „ziel.test“ auf das Ziel.
func setup(t *testing.T, g *gate) (client *http.Client, target, tlsTarget *httptest.Server, searx *httptest.Server) {
	t.Helper()
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hallo von "+r.Host+r.URL.Path)
	}))
	tlsTarget = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "sicher") }))
	searx = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[],"q":"`+r.URL.Query().Get("q")+`"}`)
	}))
	t.Cleanup(func() { target.Close(); tlsTarget.Close(); searx.Close() })
	su, _ := url.Parse(searx.URL)
	p := &Proxy{Gate: g, Searx: su, HTTPPort: portOf(t, target.URL), HTTPSPort: portOf(t, tlsTarget.URL),
		Allow: func(ip net.IP) bool { return ip.IsLoopback() },
		Lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			switch host {
			case "ziel.test":
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			case "intern.test":
				return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	pu, _ := url.Parse(ps.URL)
	client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	return
}

func TestGate(t *testing.T) {
	g := &gate{}
	c, target, _, _ := setup(t, g)
	u := "http://ziel.test:" + strconv.Itoa(portOf(t, target.URL)) + "/x"
	if resp, err := c.Get(u); err != nil || resp.StatusCode != 403 {
		t.Fatalf("unbekannte Quelle: %v %v", resp, err)
	}
	if len(g.recs) != 0 {
		t.Fatal("unbekannte Quelle protokolliert")
	}
	g.known = true
	resp, err := c.Get(u)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("Internet aus: %v %v", resp, err)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "internet access is off") || g.last().Denied == "" {
		t.Fatalf("Grund: %q %+v", b, g.last())
	}
}

func TestForwardAndBlock(t *testing.T) {
	g := &gate{known: true, internet: true}
	c, target, tlsTarget, _ := setup(t, g)
	port := strconv.Itoa(portOf(t, target.URL))
	resp, err := c.Get("http://ziel.test:" + port + "/seite")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("http: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hallo von ziel.test:"+port+"/seite" {
		t.Fatalf("Antwort: %q", b)
	}
	if r := g.last(); r.Host != "ziel.test" || r.Status != 200 || r.BytesDown == 0 || r.Path != "/seite" || r.Denied != "" {
		t.Fatalf("Protokoll: %+v", r)
	}
	// HTTPS über CONNECT
	resp, err = c.Get("https://ziel.test:" + strconv.Itoa(portOf(t, tlsTarget.URL)) + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("https: %v %v", resp, err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "sicher" {
		t.Fatalf("https-Antwort: %q", b)
	}
	c.CloseIdleConnections()
	for _, bad := range []string{
		"http://intern.test:" + port + "/",     // interne Adresse
		"http://10.0.0.7:" + port + "/",        // interne Adresse direkt
		"http://ziel.test:" + port + "1/",      // anderer Port
		"http://gibtsnicht.test:" + port + "/", // nicht auflösbar
	} {
		if resp, err := c.Get(bad); err != nil || resp.StatusCode != 403 {
			t.Errorf("%s: %v %v", bad, resp, err)
		}
	}
}

// Node tunnelt mit NODE_USE_ENV_PROXY auch HTTP per CONNECT: zu SearXNG und zu Port 80.
func TestConnectForHTTP(t *testing.T) {
	g := &gate{known: true, internet: true}
	c, target, _, _ := setup(t, g)
	pu := c.Transport.(*http.Transport).Proxy
	proxyURL, _ := pu(&http.Request{URL: &url.URL{Scheme: "https", Host: "x"}})
	for _, tc := range []struct{ host, want string }{
		{"searxng:8080", `"q":"tunnel"`},
		{"ziel.test:" + strconv.Itoa(portOf(t, target.URL)), "hallo von"},
	} {
		conn, err := net.Dial("tcp", proxyURL.Host)
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
	g := &gate{known: true, internet: true}
	c, _, _, _ := setup(t, g)
	resp, err := c.Get("http://searxng:8080/search?q=schwein&format=json")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("searxng: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"q":"schwein"`) {
		t.Fatalf("Antwort: %q", b)
	}
	// Ohne Internet auch keine Suche.
	g.mu.Lock()
	g.internet = false
	g.mu.Unlock()
	if resp, _ := c.Get("http://searxng:8080/search?q=x"); resp.StatusCode != 403 {
		t.Fatalf("Suche ohne Internet: %d", resp.StatusCode)
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
			t.Errorf("%s: öffentlich = %v", ip, got)
		}
	}
}

// Ein offener Tunnel schließt sich, sobald der Chat kein Internet mehr hat.
func TestTunnelClosedWhenInternetOff(t *testing.T) {
	old := RecheckEvery
	RecheckEvery = 50 * time.Millisecond
	defer func() { RecheckEvery = old }()
	g := &gate{known: true, internet: true}
	c, target, _, _ := setup(t, g)
	proxyURL, _ := c.Transport.(*http.Transport).Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "x"}})
	host := "ziel.test:" + strconv.Itoa(portOf(t, target.URL))
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	br := bufio.NewReader(conn)
	if resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	g.mu.Lock()
	g.internet = false
	g.mu.Unlock()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Tunnel nicht geschlossen: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(g.last().Denied, "internet switched off") {
		if time.Now().After(deadline) {
			t.Fatalf("Protokoll: %+v", g.last())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

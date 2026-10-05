// Package webproxy ist der Ausgang des Containers von pi ins Web, für die Werkzeuge web_search und
// web_extract (pi-searxng-suite). pi hat kein Netz außer dem Platz-Netz zum Orchestrator; sein Node
// schickt HTTP und HTTPS über diesen Proxy (NODE_USE_ENV_PROXY, HTTP_PROXY/HTTPS_PROXY).
//
// Der Proxy lässt eine Anfrage nur durch, wenn
//   - die Quelladresse einem aktiven Platz gehört (Zuordnung wie am LLM-Proxy),
//   - der Chat Internet hat (derselbe Schalter wie für die Ausführungs-Sandbox),
//   - das Ziel eine öffentliche Adresse auf Port 80 (HTTP) oder 443 (CONNECT) ist, oder der eigene
//     SearXNG-Dienst (Name „searxng“, intern weitergeleitet).
//
// Aufgelöst wird hier, und verbunden wird mit genau der geprüften Adresse (kein DNS-Rebinding).
// Jede Anfrage, auch eine abgewiesene, wird protokolliert (web_requests).
package webproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Gate ist der Manager: Zuordnung, Internet-Schalter und Protokoll.
type Gate interface {
	// WebAccess: Chat und Platz zur Quelladresse und ob der Chat Internet hat (chat leer: unbekannt).
	WebAccess(ip string) (chat, slot string, internet bool)
	// RecordWeb trägt eine Anfrage ein und liefert ihre Kennung (0: nicht gespeichert). Ein Tunnel
	// wird beim Aufbau eingetragen und beim Schließen mit FinishWeb ergänzt (Bytes, Dauer).
	RecordWeb(r Request) int64
	FinishWeb(id int64, r Request)
}

// Request ist ein Protokolleintrag.
type Request struct {
	ChatID, SlotID, SourceIP string
	Method, Host             string
	Port                     int
	Path                     string // nur bei HTTP; bei CONNECT leer (verschlüsselt)
	Status                   int
	BytesUp, BytesDown       int64
	Denied                   string // Grund der Abweisung, sonst leer
	StartedAt                time.Time
	DurationMs               int64
}

// SearxHost ist der Name, unter dem pi den eigenen SearXNG-Dienst anspricht (SEARXNG_URL).
const SearxHost = "searxng"

// Grenzen eines Tunnels (CONNECT). RecheckEvery: So oft prüft ein offener Tunnel den Internet-Schalter
// erneut und schließt sich, sobald er aus ist (Node hält Tunnel offen und schickt weitere Anfragen
// hindurch; Sicherheitsreview 30.09.2026).
var (
	TunnelMax    = 5 * time.Minute
	DialTimout   = 10 * time.Second
	RecheckEvery = 2 * time.Second
)

type Proxy struct {
	Gate    Gate
	Searx   *url.URL     // Adresse des SearXNG-Dienstes aus Sicht des Orchestrators (nil: keiner)
	Blocked []*net.IPNet // zusätzlich gesperrte Netze (Netze des Stacks)
	// Lookup löst Namen auf (Tests setzen ihn); nil: net.DefaultResolver.
	Lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	// Nur für Tests: erlaubte Ports (0: 80 und 443) und eine eigene Prüfung der Zieladresse.
	HTTPPort, HTTPSPort int
	Allow               func(net.IP) bool
}

func (p *Proxy) ports() (int, int) {
	h, s := p.HTTPPort, p.HTTPSPort
	if h == 0 {
		h = 80
	}
	if s == 0 {
		s = 443
	}
	return h, s
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	src, _, _ := net.SplitHostPort(r.RemoteAddr)
	rec := Request{SourceIP: src, Method: r.Method, StartedAt: start}
	chat, slot, internet := p.Gate.WebAccess(src)
	rec.ChatID, rec.SlotID = chat, slot
	host, port := hostPort(r)
	rec.Host, rec.Port = host, port
	if r.Method != http.MethodConnect {
		rec.Path = r.URL.Path
	}
	deny := func(code int, why string) {
		rec.Status, rec.Denied, rec.DurationMs = code, why, time.Since(start).Milliseconds()
		if chat != "" {
			p.Gate.RecordWeb(rec)
		}
		slog.Warn("Web-Proxy: abgewiesen", "chat", chat, "von", src, "ziel", host, "grund", why)
		http.Error(w, why, code)
	}
	switch {
	case chat == "":
		deny(http.StatusForbidden, "request not assignable to a chat")
		return
	case !internet:
		deny(http.StatusForbidden, "internet access is off for this chat; ask the user to allow it (agw-internet or mcp_request_internet)")
		return
	case host == "":
		deny(http.StatusBadRequest, "no target host")
		return
	}
	if strings.EqualFold(host, SearxHost) {
		if p.Searx == nil {
			deny(http.StatusServiceUnavailable, "search service not configured")
			return
		}
		// Node tunnelt mit NODE_USE_ENV_PROXY auch einfaches HTTP per CONNECT.
		if r.Method == http.MethodConnect {
			p.tunnel(w, r, &rec, p.Searx.Host)
			return
		}
		p.forward(w, r, &rec, p.Searx.Host, p.Searx.Scheme, p.Searx.Host)
		return
	}
	httpPort, httpsPort := p.ports()
	if r.Method == http.MethodConnect {
		// Port 443 für HTTPS, 80 für HTTP, das Node ebenfalls per CONNECT tunnelt.
		if port != httpsPort && port != httpPort {
			deny(http.StatusForbidden, "only ports 80 and 443 are allowed")
			return
		}
	} else if r.URL.Scheme != "http" || port != httpPort {
		deny(http.StatusForbidden, "only http on port 80 or https on port 443 is allowed")
		return
	}
	ip, err := p.publicIP(r.Context(), host)
	if err != nil {
		deny(http.StatusForbidden, err.Error())
		return
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, &rec, addr)
		return
	}
	p.forward(w, r, &rec, addr, "http", r.URL.Host)
}

// hostPort: Ziel der Anfrage (CONNECT: r.Host, sonst die absolute URL).
func hostPort(r *http.Request) (string, int) {
	hp := r.Host
	def := 443
	if r.Method != http.MethodConnect {
		hp, def = r.URL.Host, 80
	}
	h, ps, err := net.SplitHostPort(hp)
	if err != nil {
		return strings.Trim(hp, "[]"), def
	}
	n, err := strconv.Atoi(ps)
	if err != nil {
		return h, -1
	}
	return h, n
}

// publicIP löst host auf und liefert die erste öffentliche Adresse. Adressen des Stacks, private,
// Loopback-, Link-Local- und ähnliche Netze sind gesperrt.
func (p *Proxy) publicIP(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !p.public(ip) {
			return nil, errors.New("target address is not public")
		}
		return ip, nil
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := lookup(lctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", host)
	}
	for _, a := range addrs {
		if !p.public(a.IP) {
			// Ein Name, der (auch) auf eine interne Adresse zeigt, ist verdächtig: ganz abweisen.
			return nil, fmt.Errorf("%s resolves to a non-public address", host)
		}
	}
	return addrs[0].IP, nil
}

var extraBlocked = mustNets("0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32")

func mustNets(cidrs ...string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func (p *Proxy) public(ip net.IP) bool {
	if p.Allow != nil {
		return p.Allow(ip)
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range append(append([]*net.IPNet(nil), extraBlocked...), p.Blocked...) {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// countingConn zählt die gelesenen und geschriebenen Bytes.
type countingConn struct {
	net.Conn
	up, down *atomic.Int64 // up: zum Ziel geschrieben, down: vom Ziel gelesen
}

func (c countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.down.Add(int64(n))
	return n, err
}

func (c countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.up.Add(int64(n))
	return n, err
}

// forward leitet eine HTTP-Anfrage (absolute URL) an addr weiter, ohne erneute Namensauflösung;
// hostHeader ist der Host, den das Ziel sieht.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, rec *Request, addr, scheme, hostHeader string) {
	var up, down atomic.Int64
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: DialTimout}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return countingConn{Conn: c, up: &up, down: &down}, nil
		},
		ResponseHeaderTimeout: 60 * time.Second,
		DisableKeepAlives:     true,
	}
	status := 0
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = addr
			pr.Out.Host = hostHeader
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status = http.StatusBadGateway
			http.Error(w, "upstream not reachable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
	rec.Status, rec.BytesUp, rec.BytesDown = status, up.Load(), down.Load()
	rec.DurationMs = time.Since(rec.StartedAt).Milliseconds()
	p.Gate.RecordWeb(*rec)
}

// tunnel verbindet für CONNECT (HTTPS) mit addr und reicht die Bytes in beide Richtungen durch.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, rec *Request, addr string) {
	dst, err := (&net.Dialer{Timeout: DialTimout}).DialContext(r.Context(), "tcp", addr)
	if err != nil {
		rec.Status, rec.Denied = http.StatusBadGateway, "upstream not reachable"
		rec.DurationMs = time.Since(rec.StartedAt).Milliseconds()
		p.Gate.RecordWeb(*rec)
		http.Error(w, "upstream not reachable", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "tunnel not supported", http.StatusInternalServerError)
		return
	}
	src, buf, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	// Node hält Tunnel offen (keep-alive): gleich eintragen, beim Schließen ergänzen.
	rec.Status, rec.DurationMs = http.StatusOK, 0
	id := p.Gate.RecordWeb(*rec)
	deadline := time.Now().Add(TunnelMax)
	_ = src.SetDeadline(deadline)
	_ = dst.SetDeadline(deadline)
	var up, down atomic.Int64
	done := make(chan struct{}, 2)
	stop := make(chan struct{})
	var revoked atomic.Bool
	go func() {
		t := time.NewTicker(RecheckEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if chat, _, on := p.Gate.WebAccess(rec.SourceIP); chat != rec.ChatID || !on {
					revoked.Store(true)
					src.Close()
					dst.Close()
					return
				}
			}
		}
	}()
	go func() {
		// Was nach dem CONNECT schon gepuffert ist, zuerst.
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			m, _ := dst.Write(b)
			up.Add(int64(m))
		}
		n, _ := io.Copy(dst, src)
		up.Add(n)
		closeWrite(dst)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(src, dst)
		down.Add(n)
		closeWrite(src)
		done <- struct{}{}
	}()
	<-done
	<-done
	close(stop)
	src.Close()
	dst.Close()
	if revoked.Load() {
		rec.Denied = "tunnel closed: internet switched off"
		slog.Info("Web-Proxy: Tunnel geschlossen, Internet aus", "chat", rec.ChatID, "ziel", rec.Host)
	}
	rec.BytesUp, rec.BytesDown = up.Load(), down.Load()
	rec.DurationMs = time.Since(rec.StartedAt).Milliseconds()
	p.Gate.FinishWeb(id, *rec)
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

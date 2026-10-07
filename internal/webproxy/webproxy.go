// Package webproxy is the exit of pi's container to the web, for the tools web_search and
// web_extract (pi-searxng-suite). pi has no network except the slot network to the orchestrator; its Node
// sends HTTP and HTTPS through this proxy (NODE_USE_ENV_PROXY, HTTP_PROXY/HTTPS_PROXY).
//
// The proxy lets a request through only if
//   - the source address belongs to an active slot (attribution as at the LLM proxy),
//   - the chat has internet (the same switch as for the execution sandbox),
//   - the target is a public address on port 80 (HTTP) or 443 (CONNECT), or our own
//     SearXNG service (name "searxng", forwarded internally).
//
// Resolution happens here, and the connection goes to exactly the checked address (no DNS rebinding).
// Every request, including a refused one, is logged (web_requests).
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

// Gate is the manager: attribution, internet switch and log.
type Gate interface {
	// WebAccess: chat and slot for the source address and whether the chat has internet (chat empty: unknown).
	WebAccess(ip string) (chat, slot string, internet bool)
	// RecordWeb records a request and returns its ID (0: not stored). A tunnel
	// is recorded when it is set up and completed with FinishWeb on close (bytes, duration).
	RecordWeb(r Request) int64
	FinishWeb(id int64, r Request)
}

// Request is a log entry.
type Request struct {
	ChatID, SlotID, SourceIP string
	Method, Host             string
	Port                     int
	Path                     string // HTTP only; empty for CONNECT (encrypted)
	Status                   int
	BytesUp, BytesDown       int64
	Denied                   string // reason for refusal, otherwise empty
	StartedAt                time.Time
	DurationMs               int64
}

// SearxHost is the name under which pi addresses our own SearXNG service (SEARXNG_URL).
const SearxHost = "searxng"

// Limits of a tunnel (CONNECT). RecheckEvery: this often an open tunnel rechecks the internet switch
// and closes as soon as it is off (Node keeps tunnels open and sends further requests
// through them; security review 2026-09-30).
var (
	TunnelMax    = 5 * time.Minute
	DialTimout   = 10 * time.Second
	RecheckEvery = 2 * time.Second
)

type Proxy struct {
	Gate    Gate
	Searx   *url.URL     // address of the SearXNG service as seen by the orchestrator (nil: none)
	Blocked []*net.IPNet // additionally blocked networks (the stack's networks)
	// Lookup resolves names (tests set it); nil: net.DefaultResolver.
	Lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	// For tests only: allowed ports (0: 80 and 443) and a custom check of the target address.
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
		slog.Warn("web proxy: refused", "chat", chat, "from", src, "target", host, "reason", why)
		http.Error(w, why, code)
	}
	switch {
	case chat == "":
		deny(http.StatusForbidden, "request not assignable to a chat")
		return
	case !internet:
		deny(http.StatusForbidden, "internet access is off for this chat; ask the user to allow it (agw-internet, mcp_request_internet or request_internet)")
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
		// With NODE_USE_ENV_PROXY Node tunnels plain HTTP through CONNECT as well.
		if r.Method == http.MethodConnect {
			p.tunnel(w, r, &rec, p.Searx.Host)
			return
		}
		p.forward(w, r, &rec, p.Searx.Host, p.Searx.Scheme, p.Searx.Host)
		return
	}
	httpPort, httpsPort := p.ports()
	if r.Method == http.MethodConnect {
		// Port 443 for HTTPS, 80 for HTTP, which Node also tunnels through CONNECT.
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

// hostPort: target of the request (CONNECT: r.Host, otherwise the absolute URL).
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

// publicIP resolves host and returns the first public address. The stack's addresses, private,
// loopback, link-local and similar networks are blocked.
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
			// A name that (also) points to an internal address is suspicious: refuse entirely.
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

// countingConn counts the bytes read and written.
type countingConn struct {
	net.Conn
	up, down *atomic.Int64 // up: written to the target, down: read from the target
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

// forward forwards an HTTP request (absolute URL) to addr without resolving the name again;
// hostHeader is the host the target sees.
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

// tunnel connects to addr for CONNECT (HTTPS) and passes the bytes through in both directions.
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
	// Node keeps tunnels open (keep-alive): record right away, complete on close.
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
		// Whatever is already buffered after the CONNECT goes first.
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
		slog.Info("web proxy: tunnel closed, internet off", "chat", rec.ChatID, "target", rec.Host)
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

// Package webproxy is the exit of pi's container to the web, for the tools web_search and
// web_extract (pi-searxng-suite). pi has no network except the slot network to the orchestrator; its Node
// sends HTTP and HTTPS through this proxy (NODE_USE_ENV_PROXY, HTTP_PROXY/HTTPS_PROXY).
//
// The proxy lets a request through only if
//   - the source address belongs to an active slot (attribution as at the LLM proxy),
//   - an internet grant of the chat covers the target (issue #175, netgrant.Decide): search (pi's tools), the listed
//     host names of a domains grant (https only, TLS server name equal to the CONNECT host) or full,
//   - the target is a public address on port 80 (HTTP) or 443 (CONNECT), or our own
//     SearXNG service (name "searxng", forwarded internally).
//
// Resolution happens here, and the connection goes to exactly the checked address (no DNS rebinding). Redirects are
// never followed: the target of a redirect is a new connection and is decided for itself. Every connection counts
// on its grant; a tunnel is rechecked every RecheckEvery and closed when the grant ends or runs over its data limit.
// Every request, including a refused one, is logged (web_requests).
package webproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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

	"agw/internal/netgrant"
)

// Gate is the manager: attribution, the decision by the chat's internet grants, the byte count and the log.
type Gate interface {
	// Attribute: chat and slot for the source address (chat empty: unknown).
	Attribute(ip string) (chat, slot string)
	// Decide checks a connection against the chat's active grants (netgrant.Decide).
	Decide(chat string, o netgrant.Origin, host string, port int) netgrant.Decision
	// Still reports whether the grant still lets traffic through; why is the reason of the end ("time limit",
	// "data limit", "installed", "switched off") when it does not.
	Still(chat string, grant int64) (ok bool, why string)
	// Count adds bytes (both directions) to the grant's data counter.
	Count(grant, n int64)
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
	// GrantID is the grant that decided the request (for a refusal: the grant that governs the chat); 0: none.
	GrantID int64
	// Origin: pi, exec or pkg. ToolCallID: the tool call a command of the sandbox belongs to (empty: unknown).
	Origin     string
	ToolCallID string
}

// SearxHost is the name under which pi addresses our own SearXNG service (SEARXNG_URL).
const SearxHost = netgrant.SearxHost

// Limits of a tunnel (CONNECT). RecheckEvery: this often an open tunnel rechecks the internet switch
// and closes as soon as it is off (Node keeps tunnels open and sends further requests
// through them; security review 2026-09-30).
var (
	TunnelMax    = 5 * time.Minute
	DialTimout   = 10 * time.Second
	RecheckEvery = 2 * time.Second
)

type Proxy struct {
	Gate Gate
	// Origin of the connections this proxy serves (default netgrant.OriginPi). AttributeReq assigns a request to
	// chat and slot when the source address does not (the sandbox's socket); nil: Gate.Attribute by address.
	Origin       netgrant.Origin
	AttributeReq func(*http.Request) (chat, slot string)
	ToolCallOf   func(*http.Request) string
	Searx        *url.URL     // address of the SearXNG service as seen by the orchestrator (nil: none)
	Blocked      []*net.IPNet // additionally blocked networks (the stack's networks)
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

func (p *Proxy) origin() netgrant.Origin {
	if p.Origin == "" {
		return netgrant.OriginPi
	}
	return p.Origin
}

// attribute assigns the request to its chat and slot.
func (p *Proxy) attribute(r *http.Request, src string) (chat, slot string) {
	if p.AttributeReq != nil {
		return p.AttributeReq(r)
	}
	return p.Gate.Attribute(src)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	src, _, _ := net.SplitHostPort(r.RemoteAddr)
	rec := Request{SourceIP: src, Method: r.Method, StartedAt: start, Origin: string(p.origin())}
	chat, slot := p.attribute(r, src)
	rec.ChatID, rec.SlotID = chat, slot
	host, port := hostPort(r)
	rec.Host, rec.Port = host, port
	if r.Method != http.MethodConnect {
		rec.Path = r.URL.Path
	}
	if p.ToolCallOf != nil {
		rec.ToolCallID = p.ToolCallOf(r)
	}
	deny := func(code int, why string) {
		rec.Status, rec.Denied, rec.DurationMs = code, why, time.Since(start).Milliseconds()
		if chat != "" {
			p.Gate.RecordWeb(rec)
		}
		slog.Warn("web proxy: refused", "chat", chat, "from", src, "target", host, "reason", why)
		http.Error(w, why, code)
	}
	if chat == "" {
		deny(http.StatusForbidden, netgrant.MsgNoChat)
		return
	}
	if host == "" {
		// Decide first, so that a refusal for a chat without access names the missing access, not the missing host.
		if d := p.Gate.Decide(chat, p.origin(), "", port); !d.Allow {
			rec.GrantID = d.GrantID
			deny(http.StatusForbidden, d.Reason)
			return
		}
		deny(http.StatusBadRequest, "no target host")
		return
	}
	// The grants speak of port 80 and 443; the ports of a test proxy (HTTPPort, HTTPSPort) map to them.
	dport := port
	if hp, sp := p.ports(); port == sp {
		dport = 443
	} else if port == hp {
		dport = 80
	}
	dec := p.Gate.Decide(chat, p.origin(), host, dport)
	rec.GrantID = dec.GrantID
	if !dec.Allow {
		deny(http.StatusForbidden, dec.Reason)
		return
	}
	// still reports whether the grant that allowed the request still holds (an ended grant or an exceeded data
	// limit closes the tunnel) and whether the request still comes from the same chat.
	still := func() (bool, string) {
		if p.AttributeReq == nil {
			if c, _ := p.Gate.Attribute(src); c != chat {
				return false, "chat gone"
			}
		}
		return p.Gate.Still(chat, dec.GrantID)
	}
	if ok, why := still(); !ok {
		deny(http.StatusForbidden, netgrant.MsgGrantEnded(dec.GrantID, why))
		return
	}
	if strings.EqualFold(host, SearxHost) {
		if p.Searx == nil {
			deny(http.StatusServiceUnavailable, "search service not configured")
			return
		}
		// With NODE_USE_ENV_PROXY Node tunnels plain HTTP through CONNECT as well.
		if r.Method == http.MethodConnect {
			p.tunnel(w, r, &rec, p.Searx.Host, tunnelOpts{grant: dec.GrantID, still: still})
			return
		}
		p.forward(w, r, &rec, p.Searx.Host, p.Searx.Scheme, p.Searx.Host, dec.GrantID)
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
		// A domains grant names a host, not an address: the TLS server name must be that host.
		opts := tunnelOpts{grant: dec.GrantID, still: still}
		if dec.Kind == netgrant.Domains {
			opts.sniHost = netgrant.NormalizeHost(host)
		}
		p.tunnel(w, r, &rec, addr, opts)
		return
	}
	p.forward(w, r, &rec, addr, "http", r.URL.Host, dec.GrantID)
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

// countingConn counts the bytes read and written, and reports them to the grant (count may be nil).
type countingConn struct {
	net.Conn
	up, down *atomic.Int64 // up: written to the target, down: read from the target
	count    func(int64)
}

func (c countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.down.Add(int64(n))
	if c.count != nil && n > 0 {
		c.count(int64(n))
	}
	return n, err
}

func (c countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.up.Add(int64(n))
	if c.count != nil && n > 0 {
		c.count(int64(n))
	}
	return n, err
}

// countWriter counts what is written through it, into n and (if set) onto the grant.
type countWriter struct {
	w     io.Writer
	n     *atomic.Int64
	count func(int64)
}

func (c countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n.Add(int64(n))
	if c.count != nil && n > 0 {
		c.count(int64(n))
	}
	return n, err
}

// forward forwards an HTTP request (absolute URL) to addr without resolving the name again;
// hostHeader is the host the target sees.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, rec *Request, addr, scheme, hostHeader string, grant int64) {
	var up, down atomic.Int64
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: DialTimout}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return countingConn{Conn: c, up: &up, down: &down, count: func(n int64) { p.Gate.Count(grant, n) }}, nil
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

// tunnelOpts are what a tunnel needs of the decision: the grant it counts on, the recheck, and for a domains grant
// the host the TLS server name must equal.
type tunnelOpts struct {
	grant   int64
	still   func() (bool, string)
	sniHost string
}

// sniTimeout is how long a client has to send its ClientHello after the proxy answered the CONNECT.
const sniTimeout = 10 * time.Second

var errSNIRead = errors.New("sni read")

// readOnlyConn lets crypto/tls parse a ClientHello from a buffer: reads come from r, writes are discarded.
type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (readOnlyConn) Write(b []byte) (int, error)        { return len(b), nil }
func (readOnlyConn) Close() error                       { return nil }
func (readOnlyConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (readOnlyConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }

// sniOf reads the TLS ClientHello from br without consuming it: the caller replays the peeked bytes to the target.
// br must hold at least 5+16384 bytes.
func sniOf(br *bufio.Reader) (string, error) {
	hdr, err := br.Peek(5)
	if err != nil || hdr[0] != 0x16 {
		return "", errors.New("not a TLS handshake")
	}
	n := int(hdr[3])<<8 | int(hdr[4])
	if n > 16384 {
		return "", errors.New("ClientHello too large")
	}
	rec, err := br.Peek(5 + n)
	if err != nil {
		return "", err
	}
	var name string
	cfg := &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		name = h.ServerName
		return nil, errSNIRead
	}}
	_ = tls.Server(readOnlyConn{r: bytes.NewReader(rec)}, cfg).Handshake()
	if name == "" {
		return "", errors.New("no server name")
	}
	return name, nil
}

// tunnel connects to addr for CONNECT (HTTPS) and passes the bytes through in both directions.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, rec *Request, addr string, o tunnelOpts) {
	count := func(n int64) { p.Gate.Count(o.grant, n) }
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
	var up, down atomic.Int64
	// What the client sent before we looked at it (and, for a domains grant, the ClientHello) goes to the target first.
	var prefix []byte
	if n := buf.Reader.Buffered(); n > 0 {
		prefix, _ = buf.Reader.Peek(n)
		prefix = append([]byte(nil), prefix...)
	}
	if o.sniHost != "" {
		// A domains grant allows one host name: the TLS server name of the first flight must be that host (a
		// missing name is refused). The ClientHello is peeked, not consumed, and replayed below.
		_ = src.SetReadDeadline(time.Now().Add(sniTimeout))
		br := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(prefix), src), 20<<10)
		sni, serr := sniOf(br)
		_ = src.SetReadDeadline(time.Time{})
		why := ""
		switch {
		case serr != nil:
			why = "blocked: no TLS server name for " + o.sniHost + " (" + serr.Error() + ")"
		case !strings.EqualFold(strings.TrimSuffix(sni, "."), o.sniHost):
			why = netgrant.MsgSNIMismatch(sni, o.sniHost)
		}
		if why != "" {
			rec.Status, rec.Denied, rec.DurationMs = http.StatusForbidden, why, time.Since(rec.StartedAt).Milliseconds()
			p.Gate.RecordWeb(*rec)
			slog.Warn("web proxy: tunnel refused", "chat", rec.ChatID, "target", rec.Host, "reason", why)
			src.Close()
			dst.Close()
			return
		}
		n := br.Buffered()
		prefix, _ = br.Peek(n)
		prefix = append([]byte(nil), prefix...)
	}
	// Node keeps tunnels open (keep-alive): record right away, complete on close.
	rec.Status, rec.DurationMs = http.StatusOK, 0
	id := p.Gate.RecordWeb(*rec)
	deadline := time.Now().Add(TunnelMax)
	_ = src.SetDeadline(deadline)
	_ = dst.SetDeadline(deadline)
	done := make(chan struct{}, 2)
	stop := make(chan struct{})
	var revoked atomic.Bool
	var revokedWhy atomic.Value
	go func() {
		t := time.NewTicker(RecheckEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if ok, why := o.still(); !ok {
					revokedWhy.Store(why)
					revoked.Store(true)
					src.Close()
					dst.Close()
					return
				}
			}
		}
	}()
	go func() {
		if len(prefix) > 0 {
			m, _ := dst.Write(prefix)
			up.Add(int64(m))
			count(int64(m))
		}
		_, _ = io.Copy(countWriter{w: dst, n: &up, count: count}, src)
		closeWrite(dst)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(countWriter{w: src, n: &down, count: count}, dst)
		closeWrite(src)
		done <- struct{}{}
	}()
	<-done
	<-done
	close(stop)
	src.Close()
	dst.Close()
	if revoked.Load() {
		why, _ := revokedWhy.Load().(string)
		rec.Denied = "tunnel closed: " + strings.TrimPrefix(netgrant.MsgGrantEnded(o.grant, why), "blocked: ")
		slog.Info("web proxy: tunnel closed, grant over", "chat", rec.ChatID, "target", rec.Host, "grant", o.grant, "why", why)
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

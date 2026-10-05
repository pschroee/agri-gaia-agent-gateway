package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/store"
)

// Requests from the sandbox networks are refused, all others let through.
func TestGuardBlocksSandboxSubnets(t *testing.T) {
	blocked, err := ParseSubnets("10.231.19.0/24, 10.231.20.0/24")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Blocked: blocked}
	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for addr, want := range map[string]int{
		"10.231.19.7:5555": 403, "10.231.20.3:1": 403, "10.231.18.1:40000": 204, "127.0.0.1:1": 204,
	} {
		r := httptest.NewRequest("GET", "/api/chats", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, expected %d", addr, w.Code, want)
		}
	}
	if _, err := ParseSubnets("not-a-network"); err == nil {
		t.Fatal("invalid range accepted")
	}
}

const testToken = "0123456789abcdef0123456789abcdef"

func authServer() http.Handler {
	s := &Server{Token: testToken, AllowedHosts: []string{"127.0.0.1:18480", "localhost:18480"}}
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
}

// K3: the API cannot be used without login, not even from a network
// that does not reveal a source address (host.docker.internal).
func TestAPIRequiresToken(t *testing.T) {
	h := authServer()
	cases := []struct {
		name   string
		mod    func(*http.Request)
		expect int
	}{
		{"without", func(r *http.Request) {}, 401},
		{"wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, 401},
		{"Bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) }, 204},
		{"Cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: testToken}) }, 204},
		{"foreign host (DNS rebinding)", func(r *http.Request) {
			r.Host = "attacker.example:18480"
			r.Header.Set("Authorization", "Bearer "+testToken)
		}, 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18480/api/chats", nil)
		c.mod(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.expect {
			t.Errorf("%s: %d, expected %d", c.name, w.Code, c.expect)
		}
	}
}

// Static UI files stay reachable without login; they contain nothing secret.
func TestStaticWithoutToken(t *testing.T) {
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/assets/x.js", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("static: %d", w.Code)
	}
}

func TestLoginSetsStrictCookie(t *testing.T) {
	s := &Server{Token: testToken, AllowedHosts: []string{"127.0.0.1:18480"}}
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/login?token="+testToken, nil)
	w := httptest.NewRecorder()
	s.login(w, r)
	c := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(c) != 1 || c[0].Value != testToken || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode ||
		c[0].Path != "/" || w.Header().Get("Location") != "/" {
		t.Fatalf("Login: %d %+v %v", w.Code, c, w.Header())
	}
	r = httptest.NewRequest("GET", "http://127.0.0.1:18480/login?token=wrong", nil)
	w = httptest.NewRecorder()
	s.login(w, r)
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: %d", w.Code)
	}
}

// Under a path prefix (AGW_PUBLIC_URL=https://app.<base>/agent), /login?token= leads to the UI under
// /agent/, and the cookie only applies there.
func TestLoginUnderBasePath(t *testing.T) {
	s := &Server{Token: testToken, Env: config.Env{BasePath: "/agent"}}
	r := httptest.NewRequest("GET", "http://app.example/login?token="+testToken, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	c := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/agent/" || len(c) != 1 || c[0].Path != "/agent/" {
		t.Fatalf("login under /agent: %d %v %+v", w.Code, w.Header(), c)
	}
}

// M1: cross-origin POSTs (CSRF) are refused, even with a valid cookie.
func TestCrossOriginPostRejected(t *testing.T) {
	h := authServer()
	r := httptest.NewRequest("POST", "http://127.0.0.1:18480/api/chats", strings.NewReader(`{}`))
	r.AddCookie(&http.Cookie{Name: CookieName, Value: testToken})
	r.Header.Set("Origin", "https://attacker.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-origin POST: %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, r)
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data:") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP: %q", csp)
	}
}

type fakeImages struct {
	chat, msg, path string
	data            []byte
	err             error
}

func (f *fakeImages) OpenImage(_ context.Context, chatID, msg, p string) (store.ChatImage, io.ReadCloser, error) {
	f.chat, f.msg, f.path = chatID, msg, p
	if f.err != nil {
		return store.ChatImage{}, nil, f.err
	}
	return store.ChatImage{ContentType: "image/png", Size: int64(len(f.data)), Path: "/workspace/plot.png"}, io.NopCloser(bytes.NewReader(f.data)), nil
}

// Display images: only with login, type from the magic bytes, nosniff, cached privately.
func TestImageEndpoint(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\ndata")
	f := &fakeImages{data: png}
	s := &Server{Token: testToken, Images: f}
	h := s.Handler()
	get := func(target string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18480"+target, nil)
		if auth {
			r.Header.Set("Authorization", "Bearer "+testToken)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	target := "/api/chats/c1/images?path=" + url.QueryEscape("/workspace/plot.png") + "&msg=resp-1"
	if w := get(target, false); w.Code != 401 {
		t.Fatalf("without login: %d", w.Code)
	}
	w := get(target, true)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("fetch: %d %q", w.Code, w.Body.String())
	}
	hd := w.Header()
	if hd.Get("Content-Type") != "image/png" || hd.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(hd.Get("Cache-Control"), "private") || !strings.HasPrefix(hd.Get("Content-Disposition"), "inline") {
		t.Fatalf("headers: %v", hd)
	}
	if f.chat != "c1" || f.msg != "resp-1" || f.path != "/workspace/plot.png" {
		t.Fatalf("passed on: %+v", f)
	}
	if w := get("/api/chats/c1/images?msg=resp-1", true); w.Code != 400 {
		t.Fatalf("without path: %d", w.Code)
	}
	f.err = chat.ErrImageUnavailable
	if w := get(target, true); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("not available: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	f.err = chat.ErrInvalid
	if w := get(target, true); w.Code != 400 {
		t.Fatalf("invalid: %d", w.Code)
	}
}

// Stopping a background task that is not (or no longer) running: 409.
func TestErrCodeBackground(t *testing.T) {
	if errCode(chat.ErrNotRunning) != http.StatusConflict {
		t.Fatal("ErrNotRunning")
	}
}

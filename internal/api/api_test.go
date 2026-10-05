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
	"agw/internal/store"
)

// Anfragen aus den Sandbox-Netzen werden abgewiesen, alle anderen durchgelassen.
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
			t.Errorf("%s: %d, erwartet %d", addr, w.Code, want)
		}
	}
	if _, err := ParseSubnets("kein-netz"); err == nil {
		t.Fatal("ungültige Angabe akzeptiert")
	}
}

const testToken = "0123456789abcdef0123456789abcdef"

func authServer() http.Handler {
	s := &Server{Token: testToken, AllowedHosts: []string{"127.0.0.1:18480", "localhost:18480"}}
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
}

// K3: Die API ist ohne Anmeldung nicht nutzbar, auch nicht aus einem Netz,
// das keine Quelladresse verrät (host.docker.internal).
func TestAPIRequiresToken(t *testing.T) {
	h := authServer()
	cases := []struct {
		name   string
		mod    func(*http.Request)
		expect int
	}{
		{"ohne", func(r *http.Request) {}, 401},
		{"falsches Bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer falsch") }, 401},
		{"Bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) }, 204},
		{"Cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: testToken}) }, 204},
		{"fremder Host (DNS-Rebinding)", func(r *http.Request) {
			r.Host = "angreifer.example:18480"
			r.Header.Set("Authorization", "Bearer "+testToken)
		}, 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18480/api/chats", nil)
		c.mod(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.expect {
			t.Errorf("%s: %d, erwartet %d", c.name, w.Code, c.expect)
		}
	}
}

// Statische UI-Dateien bleiben ohne Anmeldung erreichbar; sie enthalten nichts Geheimes.
func TestStaticWithoutToken(t *testing.T) {
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/assets/x.js", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("statisch: %d", w.Code)
	}
}

func TestLoginSetsStrictCookie(t *testing.T) {
	s := &Server{Token: testToken, AllowedHosts: []string{"127.0.0.1:18480"}}
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/login?token="+testToken, nil)
	w := httptest.NewRecorder()
	s.login(w, r)
	c := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(c) != 1 || c[0].Value != testToken || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("Login: %d %+v", w.Code, c)
	}
	r = httptest.NewRequest("GET", "http://127.0.0.1:18480/login?token=falsch", nil)
	w = httptest.NewRecorder()
	s.login(w, r)
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("falsches Token: %d", w.Code)
	}
}

// M1: Cross-Origin-POSTs (CSRF) werden abgewiesen, auch mit gültigem Cookie.
func TestCrossOriginPostRejected(t *testing.T) {
	h := authServer()
	r := httptest.NewRequest("POST", "http://127.0.0.1:18480/api/chats", strings.NewReader(`{}`))
	r.AddCookie(&http.Cookie{Name: CookieName, Value: testToken})
	r.Header.Set("Origin", "https://angreifer.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("Cross-Origin-POST: %d", w.Code)
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

// Anzeige-Bilder: nur mit Anmeldung, Typ aus den Magic Bytes, nosniff, privat zwischengespeichert.
func TestImageEndpoint(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nDaten")
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
		t.Fatalf("ohne Anmeldung: %d", w.Code)
	}
	w := get(target, true)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("Abruf: %d %q", w.Code, w.Body.String())
	}
	hd := w.Header()
	if hd.Get("Content-Type") != "image/png" || hd.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(hd.Get("Cache-Control"), "private") || !strings.HasPrefix(hd.Get("Content-Disposition"), "inline") {
		t.Fatalf("Kopfzeilen: %v", hd)
	}
	if f.chat != "c1" || f.msg != "resp-1" || f.path != "/workspace/plot.png" {
		t.Fatalf("weitergereicht: %+v", f)
	}
	if w := get("/api/chats/c1/images?msg=resp-1", true); w.Code != 400 {
		t.Fatalf("ohne Pfad: %d", w.Code)
	}
	f.err = chat.ErrImageUnavailable
	if w := get(target, true); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("nicht verfügbar: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	f.err = chat.ErrInvalid
	if w := get(target, true); w.Code != 400 {
		t.Fatalf("ungültig: %d", w.Code)
	}
}

// Stopp einer Hintergrundaufgabe, die nicht (mehr) läuft: 409.
func TestErrCodeBackground(t *testing.T) {
	if errCode(chat.ErrNotRunning) != http.StatusConflict {
		t.Fatal("ErrNotRunning")
	}
}

package oidc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agw/internal/oidc"
	"agw/internal/oidc/oidctest"
)

// gateway starts an orchestrator stand-in: /oidc/… from the service, /whoami shows the session's user.
func gateway(t *testing.T, is *oidctest.Issuer, mod func(*oidc.Config)) (*oidc.Service, *httptest.Server) {
	t.Helper()
	var svc *oidc.Service
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := oidc.Config{Issuer: is.IssuerURL(), ClientID: is.ClientID, ClientSecret: is.ClientSecret, PublicURL: srv.URL}
	if mod != nil {
		mod(&cfg)
	}
	svc, err := oidc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/oidc/", svc.Handler())
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		u, ok := svc.SessionUser(r)
		if !ok {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, u.Sub+" "+u.Username)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "UI "+r.URL.RequestURI()) })
	return svc, srv
}

func get(t *testing.T, b *oidctest.Browser, u string) (int, string) {
	t.Helper()
	resp, err := b.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

var anna = oidctest.User{Sub: "sub-anna", Username: "anna", Name: "Anna A."}

func TestLoginFlow(t *testing.T) {
	is := oidctest.New(t)
	svc, srv := gateway(t, is, nil)
	b := oidctest.NewBrowser(t)
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatalf("without login: %d", code)
	}
	is.Login(&anna)
	code, body := get(t, b, srv.URL+"/oidc/login?return="+url.QueryEscape("/?embed=1"))
	if code != 200 || body != "UI /?embed=1" {
		t.Fatalf("after login: %d %q", code, body)
	}
	if code, body := get(t, b, srv.URL+"/whoami"); code != 200 || body != "sub-anna anna" {
		t.Fatalf("whoami: %d %q", code, body)
	}
	tok, err := svc.AccessToken(context.Background(), "sub-anna")
	if err != nil || tok == "" {
		t.Fatalf("AccessToken: %q %v", tok, err)
	}
	// Log out: POST from the same origin.
	req, _ := http.NewRequest("POST", srv.URL+"/oidc/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := b.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("logout: %v %v", resp, err)
	}
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
	if _, err := svc.AccessToken(context.Background(), "sub-anna"); !errors.Is(err, oidc.ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestLogoutCrossOriginRejected(t *testing.T) {
	is := oidctest.New(t)
	_, srv := gateway(t, is, nil)
	req, _ := http.NewRequest("POST", srv.URL+"/oidc/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("cross-site logout: %v %v", resp, err)
	}
}

func TestPromptNoneWithoutKeycloakSession(t *testing.T) {
	is := oidctest.New(t)
	_, srv := gateway(t, is, nil)
	b := oidctest.NewBrowser(t)
	code, body := get(t, b, srv.URL+"/oidc/login?prompt=none")
	if code != 401 || !strings.Contains(body, "Not logged in. Please log in to the platform.") ||
		!strings.Contains(body, `href="/oidc/login" target="_blank"`) {
		t.Fatalf("prompt=none without session: %d %s", code, body)
	}
}

func TestCallbackRejectsForgedState(t *testing.T) {
	is := oidctest.New(t)
	_, srv := gateway(t, is, nil)
	b := oidctest.NewBrowser(t)
	is.Login(&anna)
	// Without the login cookie (CSRF: code of another browser slipped in).
	code, body := get(t, b, srv.URL+"/oidc/callback?code=abc&state=foreign")
	if code != 400 || !strings.Contains(body, "expired or invalid") {
		t.Fatalf("foreign state: %d %s", code, body)
	}
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatal("session despite foreign state")
	}
}

func TestReturnPathIsLocal(t *testing.T) {
	is := oidctest.New(t)
	_, srv := gateway(t, is, nil)
	is.Login(&anna)
	for _, ret := range []string{"//evil.example/x", "https://evil.example/", "/oidc/login", "/\\evil"} {
		b := oidctest.NewBrowser(t)
		code, body := get(t, b, srv.URL+"/oidc/login?return="+url.QueryEscape(ret))
		if code != 200 || body != "UI /" {
			t.Errorf("return %q: %d %q", ret, code, body)
		}
	}
}

func TestRefreshAndRevoke(t *testing.T) {
	is := oidctest.New(t)
	is.AccessTTL = 20 * time.Second // below the threshold: every fetch renews
	svc, srv := gateway(t, is, nil)
	b := oidctest.NewBrowser(t)
	is.Login(&anna)
	get(t, b, srv.URL+"/oidc/login")
	ctx := context.Background()
	t1, err := svc.AccessToken(ctx, "sub-anna")
	if err != nil {
		t.Fatal(err)
	}
	if is.Refreshes == 0 {
		t.Fatal("token not renewed shortly before expiry")
	}
	t2, err := svc.AccessToken(ctx, "sub-anna")
	if err != nil || t2 == "" {
		t.Fatal(err)
	}
	_ = t1
	// Keycloak ends the session: the renewal fails, the session ends.
	is.Revoke("sub-anna")
	if _, err := svc.AccessToken(ctx, "sub-anna"); !errors.Is(err, oidc.ErrNoSession) {
		t.Fatalf("after revocation: %v", err)
	}
	if svc.Sessions() != 0 {
		t.Fatalf("session not discarded: %d", svc.Sessions())
	}
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatal("UI session lives on after revocation")
	}
}

func TestNewValidates(t *testing.T) {
	for _, c := range []oidc.Config{
		{Issuer: "", ClientID: "a", ClientSecret: "s", PublicURL: "https://x"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "", PublicURL: "https://x"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "s", PublicURL: "https://x/a/../b"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "s", PublicURL: "https://x/agent?x=1"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "s", PublicURL: "https://x/a%20b"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "s", PublicURL: "https://x//agent"},
	} {
		if _, err := oidc.New(c); err == nil {
			t.Errorf("accepted: %+v", c)
		}
	}
}

// prefixed starts the orchestrator stand-in behind a proxy that strips /agent (like Traefik with
// stripprefix). Outside /agent the "platform" answers.
func prefixed(t *testing.T, is *oidctest.Issuer) (*oidc.Service, *httptest.Server) {
	t.Helper()
	inner := http.NewServeMux()
	outer := http.NewServeMux()
	srv := httptest.NewServer(outer)
	t.Cleanup(srv.Close)
	svc, err := oidc.New(oidc.Config{Issuer: is.IssuerURL(), ClientID: is.ClientID, ClientSecret: is.ClientSecret, PublicURL: srv.URL + "/agent/"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Base() != "/agent" {
		t.Fatalf("Base: %q", svc.Base())
	}
	inner.Handle("/oidc/", svc.Handler())
	inner.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		if u, ok := svc.SessionUser(r); ok {
			io.WriteString(w, u.Sub)
			return
		}
		w.WriteHeader(401)
	})
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "UI "+r.URL.RequestURI()) })
	outer.Handle("/agent/", http.StripPrefix("/agent", inner))
	outer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "Platform "+r.URL.RequestURI()) })
	return svc, srv
}

func TestLoginFlowUnderPrefix(t *testing.T) {
	is := oidctest.New(t)
	_, srv := prefixed(t, is)
	b := oidctest.NewBrowser(t)
	is.Login(&anna)
	code, body := get(t, b, srv.URL+"/agent/oidc/login?return="+url.QueryEscape("/agent/?embed=1#/chats/x"))
	if code != 200 || body != "UI /?embed=1" {
		t.Fatalf("after login: %d %q", code, body)
	}
	if code, body := get(t, b, srv.URL+"/agent/whoami"); code != 200 || body != "sub-anna" {
		t.Fatalf("whoami: %d %q", code, body)
	}
	// The session cookie only applies under /agent/; the platform on the same host does not see it.
	pu, _ := url.Parse(srv.URL + "/")
	for _, c := range b.Jar.Cookies(pu) {
		t.Errorf("cookie visible outside /agent: %s", c.Name)
	}
	// Without return it goes to the UI's start page under /agent/.
	b2 := oidctest.NewBrowser(t)
	if code, body := get(t, b2, srv.URL+"/agent/oidc/login"); code != 200 || body != "UI /" {
		t.Fatalf("without return: %d %q", code, body)
	}
}

func TestReturnPathUnderPrefix(t *testing.T) {
	is := oidctest.New(t)
	_, srv := prefixed(t, is)
	is.Login(&anna)
	for ret, want := range map[string]string{
		"/agent/":                     "UI /",
		"/agent/?embed=1":             "UI /?embed=1",
		"/":                           "UI /", // platform start page: not our path
		"/agentx/":                    "UI /",
		"/agent/../evil":              "UI /",
		"/agent/%2e%2e/evil":          "UI /",
		"/agent/oidc/login":           "UI /",
		"//evil.example/agent/":       "UI /",
		"https://evil.example/":       "UI /",
		"https://evil.example/agent/": "UI /",
		"/agent/\\evil":               "UI /",
	} {
		b := oidctest.NewBrowser(t)
		code, body := get(t, b, srv.URL+"/agent/oidc/login?return="+url.QueryEscape(ret))
		if code != 200 || body != want {
			t.Errorf("return %q: %d %q, expected %q", ret, code, body, want)
		}
	}
}

func TestPromptNoneUnderPrefixLinksToPrefixedLogin(t *testing.T) {
	is := oidctest.New(t)
	_, srv := prefixed(t, is)
	b := oidctest.NewBrowser(t)
	code, body := get(t, b, srv.URL+"/agent/oidc/login?prompt=none&return="+url.QueryEscape("/agent/?embed=1"))
	if code != 401 || !strings.Contains(body, `href="/agent/oidc/login?return=%2Fagent%2F%3Fembed%3D1" target="_blank"`) {
		t.Fatalf("prompt=none under /agent: %d %s", code, body)
	}
}

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

// gateway startet einen Orchestrator-Ersatz: /oidc/… vom Dienst, /whoami zeigt den Nutzer der Sitzung.
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
		t.Fatalf("ohne Anmeldung: %d", code)
	}
	is.Login(&anna)
	code, body := get(t, b, srv.URL+"/oidc/login?return="+url.QueryEscape("/?embed=1"))
	if code != 200 || body != "UI /?embed=1" {
		t.Fatalf("nach der Anmeldung: %d %q", code, body)
	}
	if code, body := get(t, b, srv.URL+"/whoami"); code != 200 || body != "sub-anna anna" {
		t.Fatalf("whoami: %d %q", code, body)
	}
	tok, err := svc.AccessToken(context.Background(), "sub-anna")
	if err != nil || tok == "" {
		t.Fatalf("AccessToken: %q %v", tok, err)
	}
	// Abmelden: POST mit gleicher Herkunft.
	req, _ := http.NewRequest("POST", srv.URL+"/oidc/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := b.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("logout: %v %v", resp, err)
	}
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatalf("nach logout: %d", code)
	}
	if _, err := svc.AccessToken(context.Background(), "sub-anna"); !errors.Is(err, oidc.ErrNoSession) {
		t.Fatalf("nach logout: %v", err)
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
	if code != 401 || !strings.Contains(body, "Nicht angemeldet. Bitte in der Plattform anmelden.") ||
		!strings.Contains(body, `href="/oidc/login" target="_blank"`) {
		t.Fatalf("prompt=none ohne Sitzung: %d %s", code, body)
	}
}

func TestCallbackRejectsForgedState(t *testing.T) {
	is := oidctest.New(t)
	_, srv := gateway(t, is, nil)
	b := oidctest.NewBrowser(t)
	is.Login(&anna)
	// Ohne Cookie der Anmeldung (CSRF: Code eines anderen Browsers untergeschoben).
	code, body := get(t, b, srv.URL+"/oidc/callback?code=abc&state=fremd")
	if code != 400 || !strings.Contains(body, "abgelaufen oder ungültig") {
		t.Fatalf("fremder state: %d %s", code, body)
	}
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatal("Sitzung trotz fremdem state")
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
	is.AccessTTL = 20 * time.Second // unter der Schwelle: jeder Abruf erneuert
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
		t.Fatal("Token kurz vor Ablauf nicht erneuert")
	}
	t2, err := svc.AccessToken(ctx, "sub-anna")
	if err != nil || t2 == "" {
		t.Fatal(err)
	}
	_ = t1
	// Keycloak beendet die Sitzung: die Erneuerung scheitert, die Sitzung endet.
	is.Revoke("sub-anna")
	if _, err := svc.AccessToken(ctx, "sub-anna"); !errors.Is(err, oidc.ErrNoSession) {
		t.Fatalf("nach Widerruf: %v", err)
	}
	if svc.Sessions() != 0 {
		t.Fatalf("Sitzung nicht verworfen: %d", svc.Sessions())
	}
	if code, _ := get(t, b, srv.URL+"/whoami"); code != 401 {
		t.Fatal("UI-Sitzung lebt nach Widerruf weiter")
	}
}

func TestNewValidates(t *testing.T) {
	for _, c := range []oidc.Config{
		{Issuer: "", ClientID: "a", ClientSecret: "s", PublicURL: "https://x"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "", PublicURL: "https://x"},
		{Issuer: "https://kc/realms/r", ClientID: "a", ClientSecret: "s", PublicURL: "https://x/pfad"},
	} {
		if _, err := oidc.New(c); err == nil {
			t.Errorf("angenommen: %+v", c)
		}
	}
}

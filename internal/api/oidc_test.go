package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/oidc"
	"agw/internal/oidc/oidctest"
	"agw/internal/pool"
	"agw/internal/store"
)

var (
	anna = oidctest.User{Sub: "sub-anna", Username: "anna", Name: "Anna A."}
	bert = oidctest.User{Sub: "sub-bert", Username: "bert", Name: "Bert B."}
)

// oidcServer startet die API im oidc-Modus gegen einen nachgebildeten Realm. m darf nil sein (dann
// nur Routen ohne Manager nutzen).
func oidcServer(t *testing.T, m *chat.Manager, p *pool.Pool[chat.Agent], cat *config.Catalog, frames []string) (*httptest.Server, *oidctest.Issuer) {
	t.Helper()
	is := oidctest.New(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	auth, err := oidc.New(oidc.Config{Issuer: is.IssuerURL(), ClientID: is.ClientID, ClientSecret: is.ClientSecret, PublicURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	h = (&Server{M: m, Pool: p, Cat: cat, OIDC: auth, FrameAncestors: frames}).Handler()
	return srv, is
}

// loggedIn meldet u über den Realm am Orchestrator an und liefert den Browser.
func loggedIn(t *testing.T, srv *httptest.Server, is *oidctest.Issuer, u oidctest.User) *oidctest.Browser {
	t.Helper()
	b := oidctest.NewBrowser(t)
	is.Login(&u)
	resp, err := b.Get(srv.URL + "/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	is.Login(nil)
	return b
}

func call(t *testing.T, b *oidctest.Browser, method, url, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestOIDCMeAndUnauthorized(t *testing.T) {
	srv, is := oidcServer(t, nil, nil, nil, []string{"https://app.agri-gaia.localhost"})
	anon := oidctest.NewBrowser(t)
	code, body := call(t, anon, "GET", srv.URL+"/api/me", "")
	if code != 401 || !strings.Contains(body, `"login":"/oidc/login"`) {
		t.Fatalf("ohne Sitzung: %d %s", code, body)
	}
	// Das API-Token gilt im oidc-Modus nicht (es ist gar keins gesetzt; ein Bearer hilft nicht).
	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("Bearer im oidc-Modus: %d", resp.StatusCode)
	}
	b := loggedIn(t, srv, is, anna)
	code, body = call(t, b, "GET", srv.URL+"/api/me", "")
	var me map[string]string
	_ = json.Unmarshal([]byte(body), &me)
	if code != 200 || me["sub"] != "sub-anna" || me["username"] != "anna" || me["name"] != "Anna A." || me["mode"] != "oidc" {
		t.Fatalf("me: %d %s", code, body)
	}
	// Einbettung: frame-ancestors aus der Einstellung.
	resp, _ := b.Get(srv.URL + "/api/me")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors https://app.agri-gaia.localhost") || strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP: %q", csp)
	}
	// /login?token= gibt es nur im token-Modus.
	nb := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, _ := nb.Get(srv.URL + "/login?token=" + testToken); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/oidc/login" ||
		len(resp.Cookies()) != 0 {
		t.Fatalf("/login im oidc-Modus: %d %v", resp.StatusCode, resp.Header)
	}
}

// prefixedOIDCServer: die API hinter einem Proxy, der /agent abschneidet (Traefik mit stripprefix), auf
// demselben Host wie die Plattform; frame-ancestors 'self' erlaubt die Einbettung in die Plattform.
func prefixedOIDCServer(t *testing.T) (*httptest.Server, *oidctest.Issuer) {
	t.Helper()
	is := oidctest.New(t)
	outer := http.NewServeMux()
	srv := httptest.NewServer(outer)
	t.Cleanup(srv.Close)
	auth, err := oidc.New(oidc.Config{Issuer: is.IssuerURL(), ClientID: is.ClientID, ClientSecret: is.ClientSecret, PublicURL: srv.URL + "/agent"})
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{OIDC: auth, FrameAncestors: []string{"'self'", srv.URL}}).Handler()
	outer.Handle("/agent/", http.StripPrefix("/agent", h))
	outer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "Plattform") })
	return srv, is
}

func TestOIDCUnderPrefix(t *testing.T) {
	srv, is := prefixedOIDCServer(t)
	anon := oidctest.NewBrowser(t)
	code, body := call(t, anon, "GET", srv.URL+"/agent/api/me", "")
	if code != 401 || !strings.Contains(body, `"login":"/agent/oidc/login"`) {
		t.Fatalf("ohne Sitzung: %d %s", code, body)
	}
	nb := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, _ := nb.Get(srv.URL + "/agent/login?token=x"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/agent/oidc/login" {
		t.Fatalf("/login unter /agent: %d %v", resp.StatusCode, resp.Header)
	}
	b := oidctest.NewBrowser(t)
	is.Login(&anna)
	resp, err := b.Get(srv.URL + "/agent/oidc/login?return=" + url.QueryEscape("/agent/?embed=1"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/agent/" || resp.Request.URL.RawQuery != "embed=1" {
		t.Fatalf("Rücksprung: %s", resp.Request.URL)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self' "+srv.URL) {
		t.Fatalf("CSP: %q", csp)
	}
	code, body = call(t, b, "GET", srv.URL+"/agent/api/me", "")
	if code != 200 || !strings.Contains(body, `"sub":"sub-anna"`) {
		t.Fatalf("me unter /agent: %d %s", code, body)
	}
}

func TestTokenModeMe(t *testing.T) {
	s := &Server{Token: testToken}
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"token"`) {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP ohne Einbettung: %q", csp)
	}
}

// ownershipEnv: echter Manager mit Postgres, Pool ohne Plätze (Anlegen scheitert nach kurzer Wartezeit).
func ownershipEnv(t *testing.T) (*chat.Manager, *pool.Pool[chat.Agent], *config.Catalog, *store.Store) {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL nicht gesetzt")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("kein Platz im Test")
	},
		func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagentsLimit: 5})
	return m, p, cat, st
}

// Chats gehören dem Nutzer: B sieht A's Chats und Bestätigungen nicht und kann darauf nichts tun.
func TestOIDCChatOwnership(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	ctx := context.Background()
	mk := func(title, owner string) store.Chat {
		c, err := st.CreateChat(ctx, store.NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetState(ctx, c.ID, store.StateDormant)
		return c
	}
	chatA, chatB, chatT := mk("von Anna", anna.Sub), mk("von Bert", bert.Sub), mk("token-Modus", "")
	apA, err := st.CreateApproval(ctx, store.Approval{ChatID: chatA.ID, Kind: "platform_write", Via: "mcp", Name: "POST /datasets", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}

	// Anlegen über die API setzt den Besitzer aus der Anmeldung, nie aus dem Körper.
	call(t, a, "POST", srv.URL+"/api/chats", `{"title":"neu von Anna","owner":"sub-bert"}`)

	titles := func(br *oidctest.Browser) string {
		code, body := call(t, br, "GET", srv.URL+"/api/chats", "")
		if code != 200 {
			t.Fatalf("Liste: %d %s", code, body)
		}
		var cs []store.Chat
		_ = json.Unmarshal([]byte(body), &cs)
		var out []string
		for _, c := range cs {
			out = append(out, c.Title)
		}
		return strings.Join(out, ",")
	}
	if got := titles(a); !strings.Contains(got, "von Anna") || !strings.Contains(got, "neu von Anna") || strings.Contains(got, "Bert") || strings.Contains(got, "token") {
		t.Fatalf("Liste von Anna: %s", got)
	}
	if got := titles(b); got != "von Bert" {
		t.Fatalf("Liste von Bert: %s", got)
	}

	// Bert auf Annas Chat: alles 404, wie ein unbekannter Chat.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/chats/" + chatA.ID, ""},
		{"GET", "/api/chats/" + chatA.ID + "/events", ""},
		{"GET", "/api/chats/" + chatA.ID + "/session", ""},
		{"GET", "/api/chats/" + chatA.ID + "/artifacts", ""},
		{"POST", "/api/chats/" + chatA.ID + "/messages", `{"text":"hallo"}`},
		{"POST", "/api/chats/" + chatA.ID + "/abort", ""},
		{"POST", "/api/chats/" + chatA.ID + "/internet", `{"enabled":true}`},
		{"DELETE", "/api/chats/" + chatA.ID + "/queue/1", ""},
		{"POST", "/api/approvals/" + apA.ID, `{"approve":true}`},
		{"GET", "/api/chats/" + chatT.ID, ""}, // ohne Besitzer: im oidc-Modus niemandes
	} {
		if code, body := call(t, b, c.method, srv.URL+c.path, c.body); code != 404 {
			t.Errorf("Bert %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	if got, _ := st.GetApproval(ctx, apA.ID); got.State != store.ApprovalPending {
		t.Fatalf("Bert hat Annas Bestätigung entschieden: %s", got.State)
	}
	for _, q := range []string{"?state=pending", "?chat=" + chatA.ID} {
		if code, body := call(t, b, "GET", srv.URL+"/api/approvals"+q, ""); code != 200 || strings.Contains(body, apA.ID) {
			t.Fatalf("Bestätigungen für Bert %s: %d %s", q, code, body)
		}
	}
	if code, _ := call(t, b, "GET", srv.URL+"/api/chats/"+chatB.ID, ""); code != 200 {
		t.Fatalf("Bert auf eigenem Chat: %d", code)
	}

	// Anna sieht und entscheidet ihre eigene Bestätigung.
	if code, body := call(t, a, "GET", srv.URL+"/api/approvals?state=pending", ""); code != 200 || !strings.Contains(body, apA.ID) {
		t.Fatalf("Bestätigungen für Anna: %d %s", code, body)
	}
	if code, body := call(t, a, "GET", srv.URL+"/api/chats/"+chatA.ID, ""); code != 200 || !strings.Contains(body, `"owner":"sub-anna"`) {
		t.Fatalf("Anna auf eigenem Chat: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/approvals/"+apA.ID, `{"approve":false}`); code != 200 {
		t.Fatalf("Anna entscheidet: %d %s", code, body)
	}
}

// Package oidctest bildet für Tests einen Keycloak-Realm nach: Discovery, JWKS, Anmeldeseite (ohne
// Formular: angemeldet ist, wer mit Login gesetzt ist), Token-Endpunkt mit Code (PKCE S256),
// refresh_token und Token-Austausch (RFC 8693, wie Keycloak 26: kein act, azp = anfragender Client).
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// User ist ein Konto im nachgebildeten Realm.
type User struct{ Sub, Username, Name string }

// Exchange ist ein Austausch, wie ihn der Token-Endpunkt gesehen hat.
type Exchange struct {
	SubjectSub string   // sub des subject_token
	Audiences  []string // angefragte Zielgruppen
	Token      string   // ausgestelltes Token
}

// Issuer ist der nachgebildete Realm.
type Issuer struct {
	*httptest.Server
	ClientID, ClientSecret string
	AccessTTL              time.Duration // Lebensdauer der Zugangstokens (Standard 5 min)
	Key                    *rsa.PrivateKey
	Kid                    string

	mu        sync.Mutex
	current   *User // in Keycloak angemeldet (nil: niemand)
	codes     map[string]codeInfo
	refresh   map[string]User
	exchanges []Exchange
	Refreshes int
	n         int
}

type codeInfo struct {
	user                       User
	nonce, challenge, redirect string
}

// New startet den Realm; Issuer-URL ist Server.URL + "/realms/test".
func New(t testing.TB) *Issuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{ClientID: "agw-agent", ClientSecret: "geheim", AccessTTL: 5 * time.Minute, Key: k, Kid: "k1",
		codes: map[string]codeInfo{}, refresh: map[string]User{}}
	is.Server = httptest.NewServer(is)
	t.Cleanup(is.Close)
	return is
}

// URL des Issuers (iss in den Tokens).
func (is *Issuer) IssuerURL() string { return is.Server.URL + "/realms/test" }

// Login meldet einen Nutzer in Keycloak an (nil: abmelden).
func (is *Issuer) Login(u *User) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.current = u
}

// Revoke beendet alle Sitzungen eines Nutzers: seine refresh_tokens gelten nicht mehr.
func (is *Issuer) Revoke(sub string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	for k, u := range is.refresh {
		if u.Sub == sub {
			delete(is.refresh, k)
		}
	}
}

// Exchanges liefert die bisherigen Austausche.
func (is *Issuer) Exchanges() []Exchange {
	is.mu.Lock()
	defer is.mu.Unlock()
	return append([]Exchange(nil), is.exchanges...)
}

// Sign stellt ein JWT mit den Angaben aus (RS256, Schlüssel des Realms).
func (is *Issuer) Sign(claims map[string]any) string {
	return is.signWith(is.Key, is.Kid, claims)
}

func (is *Issuer) signWith(k *rsa.PrivateKey, kid string, claims map[string]any) string {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	p, _ := json.Marshal(claims)
	in := enc.EncodeToString(h) + "." + enc.EncodeToString(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + enc.EncodeToString(sig)
}

// SignForeign stellt ein JWT mit einem fremden Schlüssel aus (für Tests der Signaturprüfung).
func (is *Issuer) SignForeign(claims map[string]any) string {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	return is.signWith(k, is.Kid, claims)
}

// AccessClaims sind die Angaben eines Zugangstokens für u, wie Keycloak sie an ClientID ausstellt.
func (is *Issuer) AccessClaims(u User) map[string]any {
	return map[string]any{"iss": is.IssuerURL(), "sub": u.Sub, "preferred_username": u.Username, "name": u.Name,
		"azp": is.ClientID, "aud": "account", "typ": "Bearer", "exp": time.Now().Add(is.AccessTTL).Unix(), "iat": time.Now().Unix()}
}

func (is *Issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	base := is.IssuerURL()
	switch r.URL.Path {
	case "/realms/test/.well-known/openid-configuration":
		writeJSON(w, 200, map[string]string{"issuer": base, "authorization_endpoint": base + "/protocol/openid-connect/auth",
			"token_endpoint": base + "/protocol/openid-connect/token", "jwks_uri": base + "/protocol/openid-connect/certs"})
	case "/realms/test/protocol/openid-connect/certs":
		pk := is.Key.PublicKey
		writeJSON(w, 200, map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": is.Kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pk.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pk.E)).Bytes())}}})
	case "/realms/test/protocol/openid-connect/auth":
		is.auth(w, r)
	case "/realms/test/protocol/openid-connect/token":
		is.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (is *Issuer) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, state := q.Get("redirect_uri"), q.Get("state")
	if q.Get("client_id") != is.ClientID || redirect == "" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "ungültige Anfrage", 400)
		return
	}
	is.mu.Lock()
	cur := is.current
	is.mu.Unlock()
	back, _ := url.Parse(redirect)
	v := url.Values{"state": {state}}
	if cur == nil {
		if q.Get("prompt") != "none" {
			http.Error(w, "Anmeldeformular (im Test nicht nachgebildet)", 200)
			return
		}
		v.Set("error", "login_required")
	} else {
		code := random()
		is.mu.Lock()
		is.codes[code] = codeInfo{user: *cur, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: redirect}
		is.mu.Unlock()
		v.Set("code", code)
	}
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	if f.Get("client_id") != is.ClientID || f.Get("client_secret") != is.ClientSecret {
		writeJSON(w, 401, map[string]string{"error": "unauthorized_client"})
		return
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.n++
	switch f.Get("grant_type") {
	case "authorization_code":
		ci, ok := is.codes[f.Get("code")]
		delete(is.codes, f.Get("code"))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || ci.redirect != f.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != ci.challenge {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		is.issue(w, ci.user, ci.nonce)
	case "refresh_token":
		u, ok := is.refresh[f.Get("refresh_token")]
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "Session not active"})
			return
		}
		delete(is.refresh, f.Get("refresh_token"))
		is.Refreshes++
		is.issue(w, u, "")
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		sub, ok := is.checkOwn(f.Get("subject_token"))
		if !ok || f.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:access_token" {
			writeJSON(w, 400, map[string]string{"error": "invalid_token"})
			return
		}
		aud := f["audience"]
		tok := is.Sign(map[string]any{"iss": is.IssuerURL(), "sub": sub, "preferred_username": sub, "azp": is.ClientID, "aud": aud,
			"exp": time.Now().Add(time.Hour).Unix(), "jti": fmt.Sprint("x", is.n)})
		is.exchanges = append(is.exchanges, Exchange{SubjectSub: sub, Audiences: aud, Token: tok})
		writeJSON(w, 200, map[string]any{"access_token": tok, "expires_in": 3600})
	default:
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
	}
}

// checkOwn prüft ein subject_token: eigene Signatur, an ClientID ausgestellt, nicht abgelaufen.
func (is *Issuer) checkOwn(tok string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&is.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return "", false
	}
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Sub string `json:"sub"`
		Azp string `json:"azp"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(pb, &c) != nil || c.Azp != is.ClientID || time.Now().Unix() > c.Exp {
		return "", false
	}
	return c.Sub, true
}

// issue stellt Zugangs-, ID- und refresh_token aus (Aufrufer hält mu).
func (is *Issuer) issue(w http.ResponseWriter, u User, nonce string) {
	access := is.Sign(is.AccessClaims(u))
	idc := map[string]any{"iss": is.IssuerURL(), "sub": u.Sub, "preferred_username": u.Username, "name": u.Name,
		"aud": is.ClientID, "azp": is.ClientID, "exp": time.Now().Add(is.AccessTTL).Unix(), "iat": time.Now().Unix()}
	if nonce != "" {
		idc["nonce"] = nonce
	}
	rt := random()
	is.refresh[rt] = u
	writeJSON(w, 200, map[string]any{"access_token": access, "id_token": is.Sign(idc), "refresh_token": rt,
		"expires_in": int(is.AccessTTL.Seconds()), "refresh_expires_in": 1800, "token_type": "Bearer"})
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Browser folgt Weiterleitungen über Orchestrator und Realm und hält Cookies, wie ein Browser.
type Browser struct {
	*http.Client
}

// NewBrowser liefert einen Client mit Cookie-Speicher, der Weiterleitungen folgt.
func NewBrowser(t testing.TB) *Browser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &Browser{Client: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

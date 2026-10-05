// Package oidc logs users in through the platform's Keycloak: authorization code flow with PKCE
// (S256) and state, confidential client (agw-agent). The user's tokens stay in the
// orchestrator; the browser only gets a session cookie with a random identifier.
//
// Sessions live in memory and are lost on a restart; the UI then logs in again silently with
// prompt=none, as long as the Keycloak session in the platform exists. The user's access token
// is the subject_token for the token exchange per chat (package platform): Keycloak
// exchanges tokens that were issued to agw-agent itself.
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"agw/internal/config"
)

// Config holds the settings from the environment (AGW_OIDC_*, AGW_PUBLIC_URL).
type Config struct {
	Issuer       string // https://keycloak.<base>/realms/<realm>
	ClientID     string
	ClientSecret string
	// PublicURL: public address of the UI, possibly with a path (https://app.<base>/agent) if a proxy
	// strips the path beforehand. Redirect URI = PublicURL + CallbackPath.
	PublicURL string
	HTTP      *http.Client
	Now       func() time.Time // for tests
}

const (
	// SessionCookie carries the session identifier (32 random bytes, base64url).
	SessionCookie = "agw_session"
	statePrefix   = "agw_oidc_" // + state: one cookie per login, two tabs do not interfere
	LoginPath     = "/oidc/login"
	CallbackPath  = "/oidc/callback"
	LogoutPath    = "/oidc/logout"

	// MaxSession: a session is valid at most this long, even if Keycloak keeps renewing.
	MaxSession = 12 * time.Hour
	stateTTL   = 10 * time.Minute
	refreshAt  = 30 * time.Second // renew the access token earlier
	leeway     = 60 * time.Second // clock skew for exp
)

// ErrNoSession: the user has no valid session (any more) at the orchestrator. The text goes to the
// agent as the error of a platform call and into the UI.
var ErrNoSession = errors.New("user login expired; open the chat in the platform")

// User is the logged-in user.
type User struct {
	Sub      string `json:"sub"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type session struct {
	mu        sync.Mutex // one renewal at a time
	user      User
	access    string
	refresh   string
	expiresAt time.Time // of the access token
	created   time.Time
	dropped   bool
}

// Service holds sessions, discovery and the issuer's keys.
type Service struct {
	cfg      Config
	base     string // path of PublicURL ("" or /agent)
	redirect string
	secure   bool

	mu       sync.Mutex
	sessions map[string]*session
	disc     *discovery
	discAt   time.Time
	keys     map[string]*rsa.PublicKey
	keysAt   time.Time
}

type discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	JWKS          string `json:"jwks_uri"`
}

// New checks the settings; Keycloak is only asked at the first login, so that the
// orchestrator also starts when Keycloak is currently unreachable.
func New(cfg Config) (*Service, error) {
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	iu, err := url.Parse(cfg.Issuer)
	if err != nil || (iu.Scheme != "https" && iu.Scheme != "http") || iu.Host == "" {
		return nil, fmt.Errorf("AGW_OIDC_ISSUER invalid: %q", cfg.Issuer)
	}
	pu, err := url.Parse(cfg.PublicURL)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" || pu.RawQuery != "" || pu.Fragment != "" || pu.User != nil {
		return nil, fmt.Errorf("AGW_PUBLIC_URL invalid (https://host or https://host/path): %q", cfg.PublicURL)
	}
	base, err := config.BasePath(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("OIDC: client ID and secret must be set")
	}
	local := pu.Hostname() == "localhost" || pu.Hostname() == "127.0.0.1"
	if pu.Scheme == "http" && !local {
		slog.Warn("AGW_PUBLIC_URL without TLS: browsers do not store the session cookie (Secure) there", "url", cfg.PublicURL)
	}
	if cfg.HTTP == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		cfg.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: tr}
	}
	hc := *cfg.HTTP
	// No call to Keycloak follows redirects: code and secret only go to the token endpoint.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &hc
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, base: base, redirect: cfg.PublicURL + CallbackPath, secure: !(pu.Scheme == "http" && local),
		sessions: map[string]*session{}, keys: map[string]*rsa.PublicKey{}}, nil
}

// Handler serves /oidc/login, /oidc/callback and /oidc/logout.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LoginPath, s.login)
	mux.HandleFunc("GET "+CallbackPath, s.callback)
	cop := http.NewCrossOriginProtection()
	mux.Handle("POST "+LogoutPath, cop.Handler(http.HandlerFunc(s.logout)))
	mux.HandleFunc("/oidc/", http.NotFound)
	return mux
}

// SessionUser returns the user for the request's session cookie. An access token close to
// expiry is renewed on the way; this also keeps the Keycloak session alive while the UI is
// open. If Keycloak refuses the renewal, the session ends.
func (s *Service) SessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	s.mu.Lock()
	ss := s.sessions[c.Value]
	s.mu.Unlock()
	if ss == nil {
		return User{}, false
	}
	if _, err := s.fresh(r.Context(), c.Value, ss); err != nil && errors.Is(err, ErrNoSession) {
		return User{}, false
	}
	return ss.user, true
}

// AccessToken returns a valid access token of user sub from their most recent session,
// renewed if needed. Without a live session: ErrNoSession.
func (s *Service) AccessToken(ctx context.Context, sub string) (string, error) {
	if sub == "" {
		return "", ErrNoSession
	}
	for {
		id, ss := s.newestSession(sub)
		if ss == nil {
			return "", ErrNoSession
		}
		tok, err := s.fresh(ctx, id, ss)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, ErrNoSession) {
			return "", err // Keycloak unreachable or similar: the session stays
		}
		// This session has been discarded; an older one may still be valid.
	}
}

func (s *Service) newestSession(sub string) (string, *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bestID string
	var best *session
	now := s.cfg.Now()
	for id, ss := range s.sessions {
		if ss.user.Sub != sub || ss.dropped || now.Sub(ss.created) > MaxSession {
			continue
		}
		if best == nil || ss.created.After(best.created) {
			bestID, best = id, ss
		}
	}
	return bestID, best
}

// fresh returns the session's access token and renews it shortly before expiry. ErrNoSession means:
// session discarded (expired, refused by Keycloak).
func (s *Service) fresh(ctx context.Context, id string, ss *session) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := s.cfg.Now()
	if ss.dropped || now.Sub(ss.created) > MaxSession {
		s.drop(id)
		return "", ErrNoSession
	}
	if ss.expiresAt.Sub(now) > refreshAt {
		return ss.access, nil
	}
	if ss.refresh == "" {
		s.drop(id)
		return "", ErrNoSession
	}
	t, status, err := s.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {ss.refresh}})
	if err != nil {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			slog.Info("session ended: Keycloak refuses the renewal", "user", ss.user.Username, "error", err)
			s.drop(id)
			return "", ErrNoSession
		}
		if ss.expiresAt.After(now) {
			return ss.access, nil // still valid; try again next time
		}
		return "", fmt.Errorf("renew access token: %w", err)
	}
	cl, err := s.verify(ctx, t.Access)
	if err == nil {
		err = s.checkAccess(cl, ss.user.Sub)
	}
	if err != nil {
		s.drop(id)
		return "", fmt.Errorf("%w (renewed token invalid: %v)", ErrNoSession, err)
	}
	ss.access, ss.expiresAt = t.Access, s.expiry(t, cl)
	if t.Refresh != "" {
		ss.refresh = t.Refresh
	}
	return ss.access, nil
}

func (s *Service) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[id]; ss != nil {
		ss.dropped = true
		delete(s.sessions, id)
	}
}

// Sessions counts the live sessions (tests, log).
func (s *Service) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// --- Login -------------------------------------------------------------------------------------

type loginState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Return   string `json:"r"`
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	d, err := s.discovery(r.Context())
	if err != nil {
		slog.Error("OIDC: discovery failed", "error", err)
		s.page(w, http.StatusBadGateway, "Login not possible: Keycloak is unreachable.", "")
		return
	}
	st := loginState{State: randomString(16), Verifier: randomString(32), Nonce: randomString(16), Return: s.safeReturn(r.URL.Query().Get("return"))}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{Name: statePrefix + st.State, Value: base64.RawURLEncoding.EncodeToString(raw), Path: s.base + "/oidc/",
		MaxAge: int(stateTTL.Seconds()), HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	sum := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.redirect},
		"scope":                 {"openid profile"},
		"state":                 {st.State},
		"nonce":                 {st.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if p := r.URL.Query().Get("prompt"); p == "none" || p == "login" {
		q.Set("prompt", p)
	}
	sep := "?"
	if strings.Contains(d.Authorization, "?") {
		sep = "&"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, d.Authorization+sep+q.Encode(), http.StatusFound)
}

// silentErrors: responses to prompt=none when nobody is logged in to Keycloak.
var silentErrors = map[string]bool{"login_required": true, "interaction_required": true, "consent_required": true, "account_selection_required": true}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateParam := q.Get("state")
	st, ok := s.takeState(w, r, stateParam)
	if e := q.Get("error"); e != "" {
		ret := s.base + "/"
		if ok {
			ret = st.Return
		}
		if silentErrors[e] {
			s.page(w, http.StatusUnauthorized, "Not logged in. Please log in to the platform.", ret)
			return
		}
		slog.Warn("OIDC: Keycloak reports an error", "error", e, "description", q.Get("error_description"))
		s.page(w, http.StatusUnauthorized, "Login failed: "+e, ret)
		return
	}
	if !ok {
		s.page(w, http.StatusBadRequest, "Login expired or invalid. Please log in again.", s.base+"/")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.page(w, http.StatusBadRequest, "Login failed: no code.", st.Return)
		return
	}
	ctx := r.Context()
	t, _, err := s.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.redirect}, "code_verifier": {st.Verifier}})
	if err != nil {
		slog.Warn("OIDC: code exchange failed", "error", err)
		s.page(w, http.StatusBadGateway, "Login failed: Keycloak did not accept the code.", st.Return)
		return
	}
	user, access, err := s.checkLogin(ctx, t, st.Nonce)
	if err != nil {
		slog.Warn("OIDC: token refused", "error", err)
		s.page(w, http.StatusUnauthorized, "Login failed: invalid token.", st.Return)
		return
	}
	id := randomString(32)
	now := s.cfg.Now()
	s.mu.Lock()
	for k, ss := range s.sessions { // clean up: expired sessions
		if now.Sub(ss.created) > MaxSession {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = &session{user: user, access: t.Access, refresh: t.Refresh, expiresAt: s.expiry(t, access), created: now}
	s.mu.Unlock()
	slog.Info("user logged in", "user", user.Username, "sub", user.Sub)
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: id, Path: s.base + "/", MaxAge: int(MaxSession.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, st.Return, http.StatusSeeOther)
}

// takeState reads the login cookie for the state parameter, checks it and deletes it.
func (s *Service) takeState(w http.ResponseWriter, r *http.Request, state string) (loginState, bool) {
	if state == "" || len(state) > 64 || strings.ContainsAny(state, "=;, ") {
		return loginState{}, false
	}
	c, err := r.Cookie(statePrefix + state)
	if err != nil {
		return loginState{}, false
	}
	http.SetCookie(w, &http.Cookie{Name: statePrefix + state, Value: "", Path: s.base + "/oidc/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return loginState{}, false
	}
	var st loginState
	if json.Unmarshal(raw, &st) != nil || st.Verifier == "" || subtle.ConstantTimeCompare([]byte(st.State), []byte(state)) != 1 {
		return loginState{}, false
	}
	st.Return = s.safeReturn(st.Return)
	return st, true
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		s.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: s.base + "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// Base is the path of AGW_PUBLIC_URL ("" or e.g. "/agent").
func (s *Service) Base() string { return s.base }

// safeReturn only allows paths under our own base on this host (no //host, no scheme,
// not …/oidc/…, no escape via ..). Otherwise it goes to the UI's start page.
func (s *Service) safeReturn(p string) string {
	home := s.base + "/"
	if p == "" || len(p) > 512 || !strings.HasPrefix(p, home) || strings.HasPrefix(p, "//") || strings.HasPrefix(p, s.base+"/oidc/") ||
		strings.ContainsAny(p, "\\\r\n\t") {
		return home
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return home
	}
	// The browser resolves ./ and ../ (also %2e%2e); the target must still be under the base afterwards.
	if c := path.Clean(u.Path); c != s.base && !strings.HasPrefix(c, home) || strings.HasPrefix(c, s.base+"/oidc/") || c == s.base+"/oidc" {
		return home
	}
	return p
}

// page shows a small page instead of the UI (refused or failed login).
func (s *Service) page(w http.ResponseWriter, code int, msg, ret string) {
	link := s.base + LoginPath
	if ret != "" && ret != s.base+"/" {
		link += "?return=" + url.QueryEscape(ret)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Agent: login</title>
<style>body{font-family:system-ui,sans-serif;margin:2rem 1rem;color:#1f2937;line-height:1.5}a{color:#0f5432}</style>
</head><body><p>%s</p><p><a href="%s" target="_blank" rel="noopener">Log in</a></p></body></html>
`, html.EscapeString(msg), html.EscapeString(link))
}

// --- Keycloak ------------------------------------------------------------------------------------

type tokenResp struct {
	Access      string `json:"access_token"`
	Refresh     string `json:"refresh_token"`
	ID          string `json:"id_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// token sends a form to the token endpoint (client_secret_post) and also returns the status.
func (s *Service) token(ctx context.Context, form url.Values) (tokenResp, int, error) {
	d, err := s.discovery(ctx)
	if err != nil {
		return tokenResp{}, 0, err
	}
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResp{}, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return tokenResp{}, 0, err
	}
	defer resp.Body.Close()
	var t tokenResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return tokenResp{}, resp.StatusCode, fmt.Errorf("token response %d unreadable", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || t.Access == "" {
		return tokenResp{}, resp.StatusCode, fmt.Errorf("Keycloak responds %d: %s %s", resp.StatusCode, t.Error, t.Description)
	}
	return t, resp.StatusCode, nil
}

func (s *Service) discovery(ctx context.Context) (*discovery, error) {
	s.mu.Lock()
	if s.disc != nil && s.cfg.Now().Sub(s.discAt) < time.Hour {
		d := s.disc
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()
	var d discovery
	if err := s.getJSON(ctx, s.cfg.Issuer+"/.well-known/openid-configuration", &d); err != nil {
		return nil, err
	}
	if strings.TrimRight(d.Issuer, "/") != s.cfg.Issuer {
		return nil, fmt.Errorf("discovery names issuer %q, configured is %q", d.Issuer, s.cfg.Issuer)
	}
	if d.Authorization == "" || d.Token == "" || d.JWKS == "" {
		return nil, errors.New("discovery incomplete (authorization_endpoint, token_endpoint, jwks_uri)")
	}
	s.mu.Lock()
	s.disc, s.discAt = &d, s.cfg.Now()
	s.mu.Unlock()
	return &d, nil
}

func (s *Service) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s responds %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// --- Token checks ------------------------------------------------------------------------------

// Claims are the verified details of a token.
type Claims struct {
	Iss      string   `json:"iss"`
	Sub      string   `json:"sub"`
	Aud      audience `json:"aud"`
	Azp      string   `json:"azp"`
	Exp      int64    `json:"exp"`
	Nonce    string   `json:"nonce"`
	Username string   `json:"preferred_username"`
	Name     string   `json:"name"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = audience{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	*a = l
	return nil
}

func (a audience) has(s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

// checkLogin checks the ID token (signature, iss, aud, azp, exp, nonce) and the access token (signature, iss,
// azp, exp, same sub) of the response to the code exchange.
func (s *Service) checkLogin(ctx context.Context, t tokenResp, nonce string) (User, Claims, error) {
	if t.ID == "" {
		return User{}, Claims{}, errors.New("no ID token (scope openid?)")
	}
	id, err := s.verify(ctx, t.ID)
	if err != nil {
		return User{}, Claims{}, fmt.Errorf("ID token: %w", err)
	}
	if !id.Aud.has(s.cfg.ClientID) {
		return User{}, Claims{}, fmt.Errorf("ID token: aud %v without %s", []string(id.Aud), s.cfg.ClientID)
	}
	if (id.Azp != "" || len(id.Aud) > 1) && id.Azp != s.cfg.ClientID {
		return User{}, Claims{}, fmt.Errorf("ID token: azp %q", id.Azp)
	}
	if subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return User{}, Claims{}, errors.New("ID token: nonce does not match")
	}
	if id.Sub == "" {
		return User{}, Claims{}, errors.New("ID token without sub")
	}
	ac, err := s.verify(ctx, t.Access)
	if err != nil {
		return User{}, Claims{}, fmt.Errorf("access token: %w", err)
	}
	if err := s.checkAccess(ac, id.Sub); err != nil {
		return User{}, Claims{}, err
	}
	u := User{Sub: id.Sub, Username: id.Username, Name: id.Name}
	if u.Username == "" {
		u.Username = ac.Username
	}
	if u.Name == "" {
		u.Name = ac.Name
	}
	return u, ac, nil
}

// checkAccess: the access token must have been issued to this client (azp), otherwise it is no good
// as a subject_token for the exchange.
func (s *Service) checkAccess(c Claims, sub string) error {
	if c.Azp != s.cfg.ClientID {
		return fmt.Errorf("access token: azp %q instead of %s", c.Azp, s.cfg.ClientID)
	}
	if c.Sub != sub {
		return fmt.Errorf("access token: sub %q does not match the ID token", c.Sub)
	}
	return nil
}

func (s *Service) expiry(t tokenResp, c Claims) time.Time {
	exp := time.Unix(c.Exp, 0)
	if t.ExpiresIn > 0 {
		if e := s.cfg.Now().Add(time.Duration(t.ExpiresIn) * time.Second); e.Before(exp) {
			exp = e
		}
	}
	return exp
}

// verify checks a JWT with RS256 against the issuer's keys, plus iss and exp.
func (s *Service) verify(ctx context.Context, tok string) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("not a JWT")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("header unreadable")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &h) != nil {
		return Claims{}, errors.New("header unreadable")
	}
	if h.Alg != "RS256" {
		return Claims{}, fmt.Errorf("algorithm %q not allowed (RS256 only)", h.Alg)
	}
	key, err := s.key(ctx, h.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("signature unreadable")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, errors.New("signature invalid")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("payload unreadable")
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return Claims{}, fmt.Errorf("payload unreadable: %w", err)
	}
	if strings.TrimRight(c.Iss, "/") != s.cfg.Issuer {
		return Claims{}, fmt.Errorf("iss %q instead of %q", c.Iss, s.cfg.Issuer)
	}
	if c.Exp == 0 || s.cfg.Now().After(time.Unix(c.Exp, 0).Add(leeway)) {
		return Claims{}, errors.New("expired")
	}
	return c, nil
}

// key returns the key for the kid; an unknown kid reloads the JWKS (at most every 10 s,
// key rotation in Keycloak).
func (s *Service) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	s.mu.Lock()
	k, ok := s.keys[kid]
	stale := s.cfg.Now().Sub(s.keysAt) > 10*time.Second
	s.mu.Unlock()
	if ok {
		return k, nil
	}
	if !stale {
		return nil, fmt.Errorf("unknown key %q", kid)
	}
	d, err := s.discovery(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := s.getJSON(ctx, d.JWKS, &set); err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jk := range set.Keys {
		if jk.Kty != "RSA" || (jk.Use != "" && jk.Use != "sig") {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(jk.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(jk.E)
		if err1 != nil || err2 != nil || len(eb) > 4 {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		pk := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
		if pk.N.BitLen() < 2048 || e < 3 {
			continue
		}
		keys[jk.Kid] = pk
	}
	s.mu.Lock()
	s.keys, s.keysAt = keys, s.cfg.Now()
	s.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key %q", kid)
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

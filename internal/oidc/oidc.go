// Package oidc meldet Nutzer über den Keycloak der Plattform an: Authorization Code Flow mit PKCE
// (S256) und state, vertraulicher Client (agw-agent). Die Tokens des Nutzers bleiben im
// Orchestrator; der Browser bekommt nur ein Sitzungs-Cookie mit einer zufälligen Kennung.
//
// Sitzungen liegen im Speicher und gehen mit einem Neustart verloren; die UI meldet sich dann mit
// prompt=none still neu an, solange die Keycloak-Sitzung in der Plattform besteht. Das Zugangstoken
// des Nutzers ist das subject_token für den Token-Austausch je Chat (Paket platform): Keycloak
// tauscht Tokens, die an agw-agent selbst ausgestellt sind.
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

// Config sind die Einstellungen aus der Umgebung (AGW_OIDC_*, AGW_PUBLIC_URL).
type Config struct {
	Issuer       string // https://keycloak.<basis>/realms/<realm>
	ClientID     string
	ClientSecret string
	// PublicURL: öffentliche Adresse der UI, auch mit Pfad (https://app.<basis>/agent), wenn ein Proxy
	// den Pfad vorher abschneidet. Redirect-URI = PublicURL + CallbackPath.
	PublicURL string
	HTTP      *http.Client
	Now       func() time.Time // für Tests
}

const (
	// SessionCookie trägt die Kennung der Sitzung (32 Zufallsbytes, base64url).
	SessionCookie = "agw_session"
	statePrefix   = "agw_oidc_" // + state: je Anmeldung ein eigenes Cookie, zwei Tabs stören sich nicht
	LoginPath     = "/oidc/login"
	CallbackPath  = "/oidc/callback"
	LogoutPath    = "/oidc/logout"

	// MaxSession: So lange gilt eine Sitzung höchstens, auch wenn Keycloak weiter erneuert.
	MaxSession = 12 * time.Hour
	stateTTL   = 10 * time.Minute
	refreshAt  = 30 * time.Second // Zugangstoken früher erneuern
	leeway     = 60 * time.Second // Uhrenabweichung bei exp
)

// ErrNoSession: Der Nutzer hat keine gültige Sitzung (mehr) am Orchestrator. Der Text geht als
// Fehler eines Plattform-Aufrufs an den Agenten und in die UI.
var ErrNoSession = errors.New("Anmeldung des Nutzers abgelaufen; Chat in der Plattform öffnen")

// User ist der angemeldete Nutzer.
type User struct {
	Sub      string `json:"sub"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type session struct {
	mu        sync.Mutex // eine Erneuerung zur Zeit
	user      User
	access    string
	refresh   string
	expiresAt time.Time // des Zugangstokens
	created   time.Time
	dropped   bool
}

// Service hält Sitzungen, Discovery und Schlüssel des Issuers.
type Service struct {
	cfg      Config
	base     string // Pfad von PublicURL ("" oder /agent)
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

// New prüft die Einstellungen; Keycloak wird erst bei der ersten Anmeldung gefragt, damit der
// Orchestrator auch startet, wenn Keycloak gerade nicht erreichbar ist.
func New(cfg Config) (*Service, error) {
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	iu, err := url.Parse(cfg.Issuer)
	if err != nil || (iu.Scheme != "https" && iu.Scheme != "http") || iu.Host == "" {
		return nil, fmt.Errorf("AGW_OIDC_ISSUER ungültig: %q", cfg.Issuer)
	}
	pu, err := url.Parse(cfg.PublicURL)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" || pu.RawQuery != "" || pu.Fragment != "" || pu.User != nil {
		return nil, fmt.Errorf("AGW_PUBLIC_URL ungültig (https://host oder https://host/pfad): %q", cfg.PublicURL)
	}
	base, err := config.BasePath(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("OIDC: Client-Kennung und Secret müssen gesetzt sein")
	}
	local := pu.Hostname() == "localhost" || pu.Hostname() == "127.0.0.1"
	if pu.Scheme == "http" && !local {
		slog.Warn("AGW_PUBLIC_URL ohne TLS: Browser speichern das Sitzungs-Cookie (Secure) dort nicht", "url", cfg.PublicURL)
	}
	if cfg.HTTP == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		cfg.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: tr}
	}
	hc := *cfg.HTTP
	// Weiterleitungen folgt kein Aufruf an Keycloak: Code und Secret gehen nur an den Token-Endpunkt.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &hc
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, base: base, redirect: cfg.PublicURL + CallbackPath, secure: !(pu.Scheme == "http" && local),
		sessions: map[string]*session{}, keys: map[string]*rsa.PublicKey{}}, nil
}

// Handler bedient /oidc/login, /oidc/callback und /oidc/logout.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LoginPath, s.login)
	mux.HandleFunc("GET "+CallbackPath, s.callback)
	cop := http.NewCrossOriginProtection()
	mux.Handle("POST "+LogoutPath, cop.Handler(http.HandlerFunc(s.logout)))
	mux.HandleFunc("/oidc/", http.NotFound)
	return mux
}

// SessionUser liefert den Nutzer zum Sitzungs-Cookie der Anfrage. Ein Zugangstoken kurz vor
// Ablauf wird dabei erneuert; das hält zugleich die Keycloak-Sitzung am Leben, solange die UI offen
// ist. Weist Keycloak die Erneuerung ab, endet die Sitzung.
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

// AccessToken liefert ein gültiges Zugangstoken des Nutzers sub aus seiner jüngsten Sitzung,
// bei Bedarf erneuert. Ohne lebende Sitzung: ErrNoSession.
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
			return "", err // Keycloak nicht erreichbar o. Ä.: die Sitzung bleibt
		}
		// Diese Sitzung ist verworfen; eine ältere kann noch gelten.
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

// fresh liefert das Zugangstoken der Sitzung und erneuert es kurz vor Ablauf. ErrNoSession heißt:
// Sitzung verworfen (abgelaufen, von Keycloak abgewiesen).
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
			slog.Info("Sitzung beendet: Keycloak weist die Erneuerung ab", "nutzer", ss.user.Username, "fehler", err)
			s.drop(id)
			return "", ErrNoSession
		}
		if ss.expiresAt.After(now) {
			return ss.access, nil // noch gültig; beim nächsten Mal erneut versuchen
		}
		return "", fmt.Errorf("Zugangstoken erneuern: %w", err)
	}
	cl, err := s.verify(ctx, t.Access)
	if err == nil {
		err = s.checkAccess(cl, ss.user.Sub)
	}
	if err != nil {
		s.drop(id)
		return "", fmt.Errorf("%w (erneuertes Token ungültig: %v)", ErrNoSession, err)
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

// Sessions zählt die lebenden Sitzungen (Tests, Protokoll).
func (s *Service) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// --- Anmeldung -------------------------------------------------------------------------------

type loginState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Return   string `json:"r"`
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	d, err := s.discovery(r.Context())
	if err != nil {
		slog.Error("OIDC: Discovery gescheitert", "fehler", err)
		s.page(w, http.StatusBadGateway, "Anmeldung nicht möglich: Keycloak ist nicht erreichbar.", "")
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

// silentErrors: Antworten auf prompt=none, wenn in Keycloak niemand angemeldet ist.
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
			s.page(w, http.StatusUnauthorized, "Nicht angemeldet. Bitte in der Plattform anmelden.", ret)
			return
		}
		slog.Warn("OIDC: Keycloak meldet einen Fehler", "fehler", e, "beschreibung", q.Get("error_description"))
		s.page(w, http.StatusUnauthorized, "Anmeldung fehlgeschlagen: "+e, ret)
		return
	}
	if !ok {
		s.page(w, http.StatusBadRequest, "Anmeldung abgelaufen oder ungültig. Bitte erneut anmelden.", s.base+"/")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.page(w, http.StatusBadRequest, "Anmeldung fehlgeschlagen: kein Code.", st.Return)
		return
	}
	ctx := r.Context()
	t, _, err := s.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.redirect}, "code_verifier": {st.Verifier}})
	if err != nil {
		slog.Warn("OIDC: Code-Tausch gescheitert", "fehler", err)
		s.page(w, http.StatusBadGateway, "Anmeldung fehlgeschlagen: Keycloak hat den Code nicht angenommen.", st.Return)
		return
	}
	user, access, err := s.checkLogin(ctx, t, st.Nonce)
	if err != nil {
		slog.Warn("OIDC: Token abgewiesen", "fehler", err)
		s.page(w, http.StatusUnauthorized, "Anmeldung fehlgeschlagen: Token ungültig.", st.Return)
		return
	}
	id := randomString(32)
	now := s.cfg.Now()
	s.mu.Lock()
	for k, ss := range s.sessions { // aufräumen: abgelaufene Sitzungen
		if now.Sub(ss.created) > MaxSession {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = &session{user: user, access: t.Access, refresh: t.Refresh, expiresAt: s.expiry(t, access), created: now}
	s.mu.Unlock()
	slog.Info("Nutzer angemeldet", "nutzer", user.Username, "sub", user.Sub)
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: id, Path: s.base + "/", MaxAge: int(MaxSession.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, st.Return, http.StatusSeeOther)
}

// takeState liest das Cookie der Anmeldung zum state-Parameter, prüft es und löscht es.
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

// Base ist der Pfad von AGW_PUBLIC_URL ("" oder etwa "/agent").
func (s *Service) Base() string { return s.base }

// safeReturn lässt nur Pfade unter der eigenen Basis auf diesem Host zu (kein //host, kein Schema,
// nicht …/oidc/…, kein Ausbruch per ..). Sonst geht es zur Startseite der UI.
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
	// Der Browser löst ./ und ../ (auch %2e%2e) auf; das Ziel muss danach noch unter der Basis liegen.
	if c := path.Clean(u.Path); c != s.base && !strings.HasPrefix(c, home) || strings.HasPrefix(c, s.base+"/oidc/") || c == s.base+"/oidc" {
		return home
	}
	return p
}

// page zeigt eine kleine Seite statt der UI (abgewiesene oder gescheiterte Anmeldung).
func (s *Service) page(w http.ResponseWriter, code int, msg, ret string) {
	link := s.base + LoginPath
	if ret != "" && ret != s.base+"/" {
		link += "?return=" + url.QueryEscape(ret)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html>
<html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Agent: Anmeldung</title>
<style>body{font-family:system-ui,sans-serif;margin:2rem 1rem;color:#1f2937;line-height:1.5}a{color:#0f5432}</style>
</head><body><p>%s</p><p><a href="%s" target="_blank" rel="noopener">Anmelden</a></p></body></html>
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

// token schickt ein Formular an den Token-Endpunkt (client_secret_post) und liefert auch den Status.
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
		return tokenResp{}, resp.StatusCode, fmt.Errorf("Token-Antwort %d unlesbar", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || t.Access == "" {
		return tokenResp{}, resp.StatusCode, fmt.Errorf("Keycloak antwortet %d: %s %s", resp.StatusCode, t.Error, t.Description)
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
		return nil, fmt.Errorf("Discovery nennt Issuer %q, eingestellt ist %q", d.Issuer, s.cfg.Issuer)
	}
	if d.Authorization == "" || d.Token == "" || d.JWKS == "" {
		return nil, errors.New("Discovery unvollständig (authorization_endpoint, token_endpoint, jwks_uri)")
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
		return fmt.Errorf("%s antwortet %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// --- Prüfung der Tokens ------------------------------------------------------------------------

// Claims sind die geprüften Angaben eines Tokens.
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

// checkLogin prüft ID-Token (Signatur, iss, aud, azp, exp, nonce) und Zugangstoken (Signatur, iss,
// azp, exp, gleicher sub) der Antwort auf den Code-Tausch.
func (s *Service) checkLogin(ctx context.Context, t tokenResp, nonce string) (User, Claims, error) {
	if t.ID == "" {
		return User{}, Claims{}, errors.New("kein ID-Token (scope openid?)")
	}
	id, err := s.verify(ctx, t.ID)
	if err != nil {
		return User{}, Claims{}, fmt.Errorf("ID-Token: %w", err)
	}
	if !id.Aud.has(s.cfg.ClientID) {
		return User{}, Claims{}, fmt.Errorf("ID-Token: aud %v ohne %s", []string(id.Aud), s.cfg.ClientID)
	}
	if (id.Azp != "" || len(id.Aud) > 1) && id.Azp != s.cfg.ClientID {
		return User{}, Claims{}, fmt.Errorf("ID-Token: azp %q", id.Azp)
	}
	if subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return User{}, Claims{}, errors.New("ID-Token: nonce passt nicht")
	}
	if id.Sub == "" {
		return User{}, Claims{}, errors.New("ID-Token ohne sub")
	}
	ac, err := s.verify(ctx, t.Access)
	if err != nil {
		return User{}, Claims{}, fmt.Errorf("Zugangstoken: %w", err)
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

// checkAccess: Das Zugangstoken muss an diesen Client ausgestellt sein (azp), sonst taugt es nicht
// als subject_token für den Austausch.
func (s *Service) checkAccess(c Claims, sub string) error {
	if c.Azp != s.cfg.ClientID {
		return fmt.Errorf("Zugangstoken: azp %q statt %s", c.Azp, s.cfg.ClientID)
	}
	if c.Sub != sub {
		return fmt.Errorf("Zugangstoken: sub %q passt nicht zum ID-Token", c.Sub)
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

// verify prüft ein JWT mit RS256 gegen die Schlüssel des Issuers sowie iss und exp.
func (s *Service) verify(ctx context.Context, tok string) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("kein JWT")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("Kopf unlesbar")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &h) != nil {
		return Claims{}, errors.New("Kopf unlesbar")
	}
	if h.Alg != "RS256" {
		return Claims{}, fmt.Errorf("Algorithmus %q nicht erlaubt (nur RS256)", h.Alg)
	}
	key, err := s.key(ctx, h.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("Signatur unlesbar")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, errors.New("Signatur ungültig")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("Nutzlast unlesbar")
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return Claims{}, fmt.Errorf("Nutzlast unlesbar: %w", err)
	}
	if strings.TrimRight(c.Iss, "/") != s.cfg.Issuer {
		return Claims{}, fmt.Errorf("iss %q statt %q", c.Iss, s.cfg.Issuer)
	}
	if c.Exp == 0 || s.cfg.Now().After(time.Unix(c.Exp, 0).Add(leeway)) {
		return Claims{}, errors.New("abgelaufen")
	}
	return c, nil
}

// key liefert den Schlüssel zur kid; eine unbekannte kid lädt die JWKS neu (höchstens alle 10 s,
// Schlüsselwechsel in Keycloak).
func (s *Service) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	s.mu.Lock()
	k, ok := s.keys[kid]
	stale := s.cfg.Now().Sub(s.keysAt) > 10*time.Second
	s.mu.Unlock()
	if ok {
		return k, nil
	}
	if !stale {
		return nil, fmt.Errorf("unbekannter Schlüssel %q", kid)
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
	return nil, fmt.Errorf("unbekannter Schlüssel %q", kid)
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand scheitert nicht
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

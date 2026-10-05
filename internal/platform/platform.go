// Package platform spricht mit der REST-API der Agri-Gaia-Plattform. Das Token
// liegt allein im Orchestrator; der Agent nennt nur Methode, Pfad, Abfrage und
// JSON-Körper, die URL baut der Client selbst aus der eingestellten Basis.
//
// Anmeldung: Der Orchestrator meldet den eingestellten Nutzer per Passwort-Grant an. Ohne
// Token-Austausch (Exchange=false) gilt dieses Token für alle Chats, über den Keycloak-Client
// „frontend“. Mit Token-Austausch (eigener vertraulicher Client, etwa „agw-agent“) tauscht der
// Orchestrator es je Chat nach RFC 8693 gegen ein eigenes Token mit eingeschränkter Zielgruppe:
// sub bleibt der Nutzer, azp nennt den Agenten (keycloak-token-austausch.md im Masterarbeits-Repo).
package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config sind die Einstellungen aus der Umgebung (AGW_PLATFORM_*).
type Config struct {
	APIURL   string // https://api.…  (leer: Anbindung aus)
	TokenURL string // …/realms/<realm>/protocol/openid-connect/token
	ClientID string
	// ClientSecret des vertraulichen Clients (leer: öffentlicher Client wie „frontend“).
	ClientSecret string
	User         string
	Password     string
	// Subject liefert, falls gesetzt, das Token des Nutzers, dem der Chat gehört (Anmeldung über
	// die Plattform, AGW_AUTH_MODE=oidc). Es ersetzt den Passwort-Grant: User und Password entfallen.
	// Mit Exchange ist es das subject_token des Austauschs, ohne Exchange geht es direkt an die API.
	Subject func(ctx context.Context, chatID string) (string, error)
	// Exchange: je Chat ein eigenes Token per Token-Austausch (verlangt ClientSecret).
	Exchange bool
	// Audiences sind die Zielgruppen des getauschten Tokens (Standard: backend, minio; MinIO
	// prüft die Zielgruppe, wenn das Backend das Token für Uploads weiterreicht).
	Audiences []string
	// ChatTokenMaxAge: So lange nutzt ein Chat sein getauschtes Token, dann wird neu getauscht
	// (Standard DefaultChatTokenMaxAge). Jeder Austausch steht im Protokoll.
	ChatTokenMaxAge time.Duration
	// OnExchange meldet jeden Austausch mit den Angaben aus dem neuen Token (für das Protokoll).
	OnExchange func(chatID string, c Claims)
	HTTP       *http.Client
	// MaxResult begrenzt den Antwortkörper, den der Agent sieht (Standard DefaultMaxResult).
	MaxResult int
}

const (
	DefaultChatTokenMaxAge = 30 * time.Minute
	DefaultMaxResult       = 64 << 10
	maxBody                = 1 << 20 // JSON-Körper vom Agenten
	maxRead                = 8 << 20 // Antwort der Plattform, vor dem Kürzen
)

// Request ist ein Aufruf, wie ihn der Agent beschreibt.
type Request struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
	// Form und Files machen den Aufruf zu multipart/form-data (Uploads von Datensätzen und
	// Modellen). Files nennt Pfade in der Ausführungs-Sandbox; gelesen werden sie vom
	// Orchestrator (Uploads), nie vom Agenten mitgeschickt.
	Form    map[string][]string `json:"form,omitempty"`
	Files   []File              `json:"files,omitempty"`
	Uploads []Upload            `json:"-"`
	// Digest verdichtet eine erfolgreiche Antwort im Orchestrator, bevor sie gekürzt wird
	// (etwa die OpenAPI-Beschreibung zur Pfadliste); vom Agenten nicht setzbar.
	Digest func([]byte) ([]byte, error) `json:"-"`
	// Full: Antwort nicht auf MaxResult kürzen (REST-Endpunkt; gekürztes JSON wäre unbrauchbar).
	// Die Obergrenze maxRead gilt weiter.
	Full bool `json:"-"`
}

// File ist eine Datei, die mit einem Aufruf hochgeladen wird: Formularfeld und Pfad in der
// Ausführungs-Sandbox.
type File struct {
	Field string `json:"field"`
	Path  string `json:"path"`
}

// Upload ist eine gelesene Datei, bereit für den Aufruf.
type Upload struct {
	Field  string
	Name   string
	Data   []byte
	SHA256 string
}

const (
	MaxFiles       = 2000      // Dateien je Aufruf
	MaxUploadBytes = 512 << 20 // Summe der Dateien je Aufruf
	maxFormValue   = 16 << 10
	maxFormTotal   = 256 << 10
)

var fieldRe = regexp.MustCompile(`^[A-Za-z0-9_\-.\[\]]{1,64}$`)

// writingGETs sind GET-Pfade, die im Backend schreiben (Review K1, am Backend-Code geprüft):
// /train/containers/{id}/model legt ein Modell samt Objekten in MinIO an, /licenses mit
// return_cached=false startet eine Analyse und setzt mit github_token eine Umgebungsvariable des
// Backends. Die HTTP-Methode allein taugt deshalb nicht als Merkmal für „schreibt“.
var writingGETs = []*regexp.Regexp{
	regexp.MustCompile(`^/train/containers/[^/]+/model$`),
	regexp.MustCompile(`^/licenses/?$`),
}

// Writes: Jeder Aufruf außer GET verändert womöglich etwas und braucht eine Bestätigung, dazu die
// bekannten schreibenden GETs. GET /train/containers gleicht nebenbei Zustände ab und entfernt
// Einträge verschwundener Container; das gilt als Lesen (README, Grenzen).
func (r Request) Writes() bool {
	if r.Method != http.MethodGet {
		return true
	}
	for _, re := range writingGETs {
		if re.MatchString(r.Path) {
			// /licenses liest nur mit return_cached (Standard true) und ohne github_token.
			if strings.HasPrefix(r.Path, "/licenses") && len(r.Query) == 0 {
				return false
			}
			return true
		}
	}
	return false
}

// String ist die Kurzform für Protokoll und Bestätigung: „POST /train/config?x=1“.
func (r Request) String() string {
	s := r.Method + " " + r.Path
	if q := r.encodedQuery(); q != "" {
		s += "?" + q
	}
	if len(r.Files) > 0 {
		s += " (multipart, " + dateien(len(r.Files)) + ")"
	} else if len(r.Form) > 0 {
		s += " (multipart)"
	}
	return s
}

// Describe zeigt Formularfelder und gelesene Dateien für die Bestätigung, vollständig.
func (r Request) Describe() string {
	var b strings.Builder
	keys := make([]string, 0, len(r.Form))
	for k := range r.Form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range r.Form[k] {
			fmt.Fprintf(&b, "%s = %s\n", k, v)
		}
	}
	if len(r.Uploads) > 0 {
		var total int
		for _, u := range r.Uploads {
			total += len(u.Data)
		}
		fmt.Fprintf(&b, "\n%s, zusammen %d Bytes:\n", dateien(len(r.Uploads)), total)
		for _, u := range r.Uploads {
			fmt.Fprintf(&b, "  %s: %s  %d Bytes  sha256 %s\n", u.Field, u.Name, len(u.Data), u.SHA256)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r Request) encodedQuery() string {
	if len(r.Query) == 0 {
		return ""
	}
	v := url.Values{}
	keys := make([]string, 0, len(r.Query))
	for k := range r.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.Set(k, r.Query[k])
	}
	return v.Encode()
}

// Result ist die Antwort an den Agenten.
type Result struct {
	Status     string `json:"status"` // ok | rejected | denied | error
	HTTPStatus int    `json:"http_status,omitempty"`
	Location   string `json:"location,omitempty"`
	Body       string `json:"body,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Message    string `json:"message,omitempty"`
	// Violation: Übergriff, der nur protokolliert und durchgelassen wurde (Delegation ohne enforce);
	// der Agent sieht ihn nicht.
	Violation string `json:"-"`
}

// RightsPath ist der Pfad, unter dem der Orchestrator selbst die übertragenen Rechte des Chats
// beantwortet (Werkzeug rights); er geht nie an die Plattform.
const RightsPath = "/_agw/rights"

// Text ist die Darstellung für MCP und CLI.
func (r Result) Text() string {
	var b strings.Builder
	switch r.Status {
	case "rejected":
		fmt.Fprintf(&b, "abgelehnt: %s", r.Message)
		return b.String()
	case "denied":
		fmt.Fprintf(&b, "verweigert vom Autorisierungsdienst: %s (welche Rechte gelten, zeigt platform_rights bzw. agw-platform rights)", r.Message)
		return b.String()
	case "error":
		fmt.Fprintf(&b, "Fehler: %s", r.Message)
		if r.HTTPStatus == 0 {
			return b.String()
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "HTTP %d", r.HTTPStatus)
	if r.Location != "" {
		fmt.Fprintf(&b, " · Location: %s", r.Location)
	}
	if r.Body != "" {
		b.WriteString("\n")
		b.WriteString(r.Body)
	}
	if r.Truncated {
		b.WriteString("\n[… gekürzt; mit skip/limit oder einem engeren Aufruf nachfragen]")
	}
	return b.String()
}

var (
	ErrNotConfigured = errors.New("Plattform-Anbindung ist nicht eingerichtet (AGW_PLATFORM_API_URL)")
	pathRe           = regexp.MustCompile(`^/[A-Za-z0-9_\-.:~/ ]*$`) // Leerzeichen: Architektur „Mask R-CNN“
	methods          = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	// Gesperrte Bereiche: /urls liefert Zugangsdaten integrierter Dienste (Fuseki-Admin,
	// rechtemodell.md im Masterarbeits-Repo), /service sind interne Rückrufe von Keycloak und Registry,
	// /users gibt das Token-Profil des Orchestrator-Kontos preis, /network liefert Passwort und
	// API-Schlüssel des Dataspace-Connectors (Review K2). Die Liste ist eine Negativliste und
	// nicht vollständig; Schlüssel mit Geheimnissen schwärzt redact zusätzlich in jeder Antwort.
	blocked = []string{"/urls", "/service", "/users", "/network"}
	// Grenzen der Abfrage, damit Bestätigung und Protokoll den Aufruf vollständig zeigen (Review W1).
	maxQueryValue = 1 << 10
	maxQueryTotal = 4 << 10
)

// Normalize prüft einen Aufruf des Agenten und bringt ihn in eine feste Form.
func Normalize(r Request) (Request, error) {
	r.Method = strings.ToUpper(strings.TrimSpace(r.Method))
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	if !methods[r.Method] {
		return r, fmt.Errorf("Methode %q nicht erlaubt (GET, POST, PUT, PATCH, DELETE)", r.Method)
	}
	if !pathRe.MatchString(r.Path) {
		return r, fmt.Errorf("Pfad %q ungültig: relativ zur API, mit / beginnend, ohne Abfrage (die gehört in query)", r.Path)
	}
	if strings.Contains(r.Path, "//") {
		return r, fmt.Errorf("Pfad %q ungültig: doppelter Schrägstrich", r.Path)
	}
	for _, seg := range strings.Split(r.Path, "/") {
		if seg == "." || seg == ".." {
			return r, fmt.Errorf("Pfad %q ungültig: . und .. sind nicht erlaubt", r.Path)
		}
	}
	total := 0
	for k, v := range r.Query {
		if len(v) > maxQueryValue || len(k) > 128 {
			return r, fmt.Errorf("Abfrageparameter %q zu lang (höchstens %d Bytes je Wert)", k, maxQueryValue)
		}
		total += len(k) + len(v)
	}
	if total > maxQueryTotal {
		return r, fmt.Errorf("Abfrage zu lang (höchstens %d Bytes)", maxQueryTotal)
	}
	if len(r.Form) > 0 || len(r.Files) > 0 {
		if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
			return r, errors.New("Formular und Dateien nur mit POST, PUT oder PATCH")
		}
		if len(r.Body) > 0 {
			return r, errors.New("entweder JSON-Körper oder Formular, nicht beides")
		}
		if len(r.Files) > MaxFiles {
			return r, fmt.Errorf("höchstens %d Dateien je Aufruf", MaxFiles)
		}
		ftotal := 0
		for k, vs := range r.Form {
			if !fieldRe.MatchString(k) {
				return r, fmt.Errorf("Formularfeld %q ungültig", k)
			}
			for _, v := range vs {
				if len(v) > maxFormValue || strings.ContainsRune(v, 0) {
					return r, fmt.Errorf("Formularfeld %q: Wert zu lang oder mit NUL", k)
				}
				ftotal += len(v)
			}
		}
		if ftotal > maxFormTotal {
			return r, errors.New("Formular zu groß")
		}
		for _, f := range r.Files {
			if !fieldRe.MatchString(f.Field) {
				return r, fmt.Errorf("Dateifeld %q ungültig", f.Field)
			}
			if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || strings.ContainsRune(f.Path, 0) || f.Path == "/" {
				return r, fmt.Errorf("Dateipfad %q ungültig: absolut und ohne . oder .. angeben", f.Path)
			}
		}
	}
	lp := strings.ToLower(r.Path)
	for _, b := range blocked {
		if lp == b || strings.HasPrefix(lp, b+"/") {
			return r, fmt.Errorf("Pfad %q ist für den Agenten gesperrt", r.Path)
		}
	}
	if len(r.Body) > 0 {
		if r.Method == http.MethodGet {
			return r, errors.New("GET nimmt keinen Körper")
		}
		if len(r.Body) > maxBody {
			return r, fmt.Errorf("Körper größer als %d KiB", maxBody>>10)
		}
		if !json.Valid(r.Body) {
			return r, errors.New("Körper ist kein gültiges JSON")
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, r.Body); err == nil {
			r.Body = buf.Bytes()
		}
		if string(r.Body) == "null" {
			r.Body = nil
		}
	}
	return r, nil
}

// Client hält das Token des Nutzers und, mit Token-Austausch, je Chat ein getauschtes Token.
type Client struct {
	cfg  Config
	base *url.URL

	mu      sync.Mutex
	access  string
	refresh string
	expires time.Time
	chats   map[string]chatToken
	xmu     sync.Mutex // ein Austausch zur Zeit, damit gleichzeitige Aufrufe eines Chats nicht doppelt tauschen
}

type chatToken struct {
	access  string
	expires time.Time // früheres von Ablauf des Tokens und ChatTokenMaxAge
}

// Claims sind die Angaben eines Tokens, die das Protokoll festhält (ungeprüft gelesen; das Token
// stammt aus der eigenen Anfrage an Keycloak).
type Claims struct {
	Sub      string    `json:"sub"`
	Username string    `json:"preferred_username"`
	Azp      string    `json:"azp"`
	Aud      []string  `json:"-"`
	Act      any       `json:"act,omitempty"`
	Exp      time.Time `json:"-"`
	JTI      string    `json:"jti"`
}

// String: „sub=test azp=agw-agent aud=backend,minio exp=…“ für das Protokoll.
func (c Claims) String() string {
	s := fmt.Sprintf("Nutzer %s, azp=%s, aud=%s, gültig bis %s", c.Username, c.Azp, strings.Join(c.Aud, ","), c.Exp.Format(time.RFC3339))
	if c.Act != nil {
		b, _ := json.Marshal(c.Act)
		s += ", act=" + string(b)
	} else {
		s += ", ohne act (Impersonation)"
	}
	return s
}

// ParseClaims liest die Nutzlast eines JWT ohne Prüfung der Signatur.
func ParseClaims(tok string) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("kein JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	var extra struct {
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, err
	}
	_ = json.Unmarshal(raw, &extra)
	if len(extra.Aud) > 0 && extra.Aud[0] == '"' {
		var a string
		_ = json.Unmarshal(extra.Aud, &a)
		c.Aud = []string{a}
	} else {
		_ = json.Unmarshal(extra.Aud, &c.Aud)
	}
	c.Exp = time.Unix(extra.Exp, 0).UTC()
	return c, nil
}

// New liefert nil, wenn keine API-URL eingestellt ist (Anbindung aus).
func New(cfg Config) (*Client, error) {
	if cfg.APIURL == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimRight(cfg.APIURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("AGW_PLATFORM_API_URL ungültig: %q", cfg.APIURL)
	}
	if cfg.Subject == nil && (cfg.TokenURL == "" || cfg.User == "" || cfg.Password == "") {
		return nil, errors.New("AGW_PLATFORM_TOKEN_URL, AGW_PLATFORM_USER und AGW_PLATFORM_PASSWORD müssen gesetzt sein")
	}
	if cfg.Subject != nil && cfg.Exchange && cfg.TokenURL == "" {
		return nil, errors.New("Token-Austausch verlangt AGW_PLATFORM_TOKEN_URL")
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "frontend"
	}
	if cfg.Exchange && cfg.ClientSecret == "" {
		return nil, errors.New("Token-Austausch (AGW_PLATFORM_TOKEN_EXCHANGE) verlangt AGW_PLATFORM_CLIENT_SECRET eines vertraulichen Clients")
	}
	if len(cfg.Audiences) == 0 {
		cfg.Audiences = []string{"backend", "minio"}
	}
	if cfg.ChatTokenMaxAge <= 0 {
		cfg.ChatTokenMaxAge = DefaultChatTokenMaxAge
	}
	if cfg.HTTP == nil {
		// Ohne Proxy aus der Umgebung; Weiterleitungen folgt weder der Token- noch der API-Aufruf,
		// sonst ginge das Passwort (307/308) oder das Token an ein fremdes Ziel (Review W4).
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		cfg.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: tr}
	}
	hc := *cfg.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &hc
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		slog.Warn("Plattform-API ohne TLS: Token geht im Klartext", "api", cfg.APIURL)
	}
	if cfg.MaxResult <= 0 {
		cfg.MaxResult = DefaultMaxResult
	}
	return &Client{cfg: cfg, base: u, chats: map[string]chatToken{}}, nil
}

// Do führt einen Aufruf aus. Die Prüfung (Normalize) läuft hier noch einmal, damit kein Aufrufer
// sie umgehen kann (Review W2). Ein Fehler der Plattform ist kein Go-Fehler, sondern ein Result mit
// Status "error".
func (c *Client) Do(ctx context.Context, chatID string, r Request) (Result, error) {
	if c == nil {
		return Result{}, ErrNotConfigured
	}
	r, err := Normalize(r)
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, nil
	}
	resp, err := c.send(ctx, chatID, r, false)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		resp, err = c.send(ctx, chatID, r, true)
	}
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRead+1))
	if err != nil {
		return Result{}, err
	}
	res := Result{Status: "ok", HTTPStatus: resp.StatusCode, Location: c.relLocation(resp.Header.Get("Location"))}
	if ct := resp.Header.Get("Content-Type"); len(raw) > 0 && !textual(ct) {
		// Downloads (ZIP, Modelle) wären im Kontext nur Rauschen (Review W3).
		res.Body = fmt.Sprintf("[Binärantwort, %d Bytes, %s; wird nicht übertragen]", len(raw), ct)
		if resp.StatusCode >= 400 {
			res.Status, res.Message = "error", fmt.Sprintf("Plattform antwortet %d", resp.StatusCode)
		}
		return res, nil
	}
	if len(raw) > maxRead {
		// Gekürztes JSON ließe sich nicht mehr schwärzen; dann geht nichts davon weiter (Review 5, W4).
		res.Truncated = true
		res.Body = fmt.Sprintf("[Antwort größer als %d MiB; nicht übertragen, weil sie sich gekürzt nicht schwärzen lässt. Mit skip/limit eingrenzen]", maxRead>>20)
		if resp.StatusCode >= 400 {
			res.Status, res.Message = "error", fmt.Sprintf("Plattform antwortet %d", resp.StatusCode)
		}
		return res, nil
	}
	if r.Digest != nil && resp.StatusCode < 300 && !res.Truncated {
		d, err := r.Digest(raw)
		if err != nil {
			return Result{}, err
		}
		raw = d
	}
	if json.Valid(raw) {
		raw = redact(raw)
	}
	if len(raw) > c.cfg.MaxResult && !r.Full {
		raw, res.Truncated = raw[:c.cfg.MaxResult], true
	}
	res.Body = strings.ToValidUTF8(string(raw), "")
	if resp.StatusCode >= 400 {
		res.Status, res.Message = "error", fmt.Sprintf("Plattform antwortet %d", resp.StatusCode)
	}
	return res, nil
}

// relLocation kürzt eine Location der Plattform auf den Pfad, damit der Agent ihn
// direkt wieder als path verwenden kann.
func (c *Client) relLocation(loc string) string {
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.Host == "" || u.Host == c.base.Host {
		p := strings.TrimPrefix(u.Path, c.base.Path)
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return p
	}
	return loc
}

func (c *Client) send(ctx context.Context, chatID string, r Request, relogin bool) (*http.Response, error) {
	tok, err := c.tokenFor(ctx, chatID, relogin)
	if err != nil {
		return nil, fmt.Errorf("Anmeldung an der Plattform: %w", err)
	}
	u := *c.base
	u.Path = c.base.Path + r.Path
	u.RawPath = ""
	u.RawQuery = r.encodedQuery()
	var body io.Reader
	ctype := ""
	switch {
	case len(r.Form) > 0 || len(r.Files) > 0:
		if len(r.Uploads) != len(r.Files) {
			return nil, errors.New("Dateien des Aufrufs sind nicht gelesen")
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		keys := make([]string, 0, len(r.Form))
		for k := range r.Form {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, v := range r.Form[k] {
				if err := mw.WriteField(k, v); err != nil {
					return nil, err
				}
			}
		}
		for _, up := range r.Uploads { // Reihenfolge wie angegeben: die Annotation ist die letzte
			fw, err := mw.CreateFormFile(up.Field, up.Name)
			if err != nil {
				return nil, err
			}
			if _, err := fw.Write(up.Data); err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		body, ctype = &buf, mw.FormDataContentType()
	case len(r.Body) > 0:
		body, ctype = bytes.NewReader(r.Body), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return c.cfg.HTTP.Do(req)
}

type tokenResp struct {
	Access     string `json:"access_token"`
	Refresh    string `json:"refresh_token"`
	ExpiresIn  int    `json:"expires_in"`
	Error      string `json:"error"`
	ErrorDescr string `json:"error_description"`
}

// token liefert ein gültiges Zugangstoken: zwischengespeichert, kurz vor Ablauf per
// refresh_token erneuert, bei force oder fehlgeschlagener Erneuerung neu angemeldet.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.access != "" && time.Until(c.expires) > 30*time.Second {
		return c.access, nil
	}
	if !force && c.refresh != "" {
		if err := c.grant(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refresh}}); err == nil {
			return c.access, nil
		}
	}
	if err := c.grant(ctx, url.Values{"grant_type": {"password"}, "username": {c.cfg.User}, "password": {c.cfg.Password}}); err != nil {
		if ctx.Err() == nil { // ein abgebrochener Aufruf verwirft nicht das Token aller (Review M1)
			c.access, c.refresh = "", ""
		}
		return "", err
	}
	return c.access, nil
}

// Forget verwirft das getauschte Token eines Chats (beim Ruhen; das Fortsetzen tauscht neu).
// Widerrufen lässt es sich nicht: Das Backend prüft Tokens nur an der Signatur.
func (c *Client) Forget(chatID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.chats, chatID)
}

// SetOnExchange setzt die Meldung je Austausch (der Manager entsteht nach dem Client).
func (c *Client) SetOnExchange(f func(chatID string, cl Claims)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.OnExchange = f
}

// Exchanging sagt, ob der Client je Chat tauscht.
func (c *Client) Exchanging() bool { return c != nil && c.cfg.Exchange }

// tokenFor liefert das Token für einen Aufruf: ohne Token-Austausch das des Nutzers, sonst das
// getauschte Token des Chats (zwischengespeichert bis ChatTokenMaxAge oder kurz vor Ablauf).
func (c *Client) tokenFor(ctx context.Context, chatID string, force bool) (string, error) {
	if !c.cfg.Exchange {
		return c.userToken(ctx, chatID, force)
	}
	if chatID == "" {
		return "", errors.New("Token-Austausch braucht einen Chat")
	}
	c.mu.Lock()
	ct, ok := c.chats[chatID]
	c.mu.Unlock()
	if ok && !force && time.Until(ct.expires) > 30*time.Second {
		return ct.access, nil
	}
	c.xmu.Lock()
	defer c.xmu.Unlock()
	c.mu.Lock()
	ct2, ok := c.chats[chatID]
	c.mu.Unlock()
	if ok && ct2.access != ct.access && time.Until(ct2.expires) > 30*time.Second {
		return ct2.access, nil // inzwischen von einem anderen Aufruf getauscht
	}
	user, err := c.userToken(ctx, chatID, force)
	if err != nil {
		return "", err
	}
	tok, err := c.exchange(ctx, user)
	if err != nil && !force && c.cfg.Subject == nil {
		// Das Nutzertoken kann serverseitig verfallen sein (Sitzung beendet): einmal neu anmelden.
		// Mit Subject gibt es kein Passwort; das Token kommt frisch aus der Sitzung des Nutzers.
		if user, err = c.token(ctx, true); err == nil {
			tok, err = c.exchange(ctx, user)
		}
	}
	if err != nil {
		return "", fmt.Errorf("Token-Austausch: %w", err)
	}
	cl, cerr := ParseClaims(tok.Access)
	exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if until := time.Now().Add(c.cfg.ChatTokenMaxAge); until.Before(exp) {
		exp = until
	}
	c.mu.Lock()
	c.chats[chatID] = chatToken{access: tok.Access, expires: exp}
	c.mu.Unlock()
	c.mu.Lock()
	notify := c.cfg.OnExchange
	c.mu.Unlock()
	if notify != nil && cerr == nil {
		notify(chatID, cl)
	}
	return tok.Access, nil
}

// userToken: das Token des Nutzers, dem der Chat gehört (Subject), sonst das des eingestellten Kontos.
func (c *Client) userToken(ctx context.Context, chatID string, force bool) (string, error) {
	if c.cfg.Subject == nil {
		return c.token(ctx, force)
	}
	if chatID == "" {
		return "", errors.New("Anmeldung des Nutzers braucht einen Chat")
	}
	return c.cfg.Subject(ctx, chatID)
}

// exchange tauscht das Nutzertoken nach RFC 8693 gegen eines mit den eingestellten Zielgruppen.
func (c *Client) exchange(ctx context.Context, subject string) (tokenResp, error) {
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {subject},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"audience":             c.cfg.Audiences,
	}
	return c.post(ctx, form)
}

func (c *Client) grant(ctx context.Context, form url.Values) error {
	t, err := c.post(ctx, form)
	if err != nil {
		return err
	}
	c.access, c.refresh = t.Access, t.Refresh
	c.expires = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return nil
}

// post schickt ein Formular an den Token-Endpunkt (mit Client-Kennung und, falls gesetzt, Secret).
func (c *Client) post(ctx context.Context, form url.Values) (tokenResp, error) {
	form.Set("client_id", c.cfg.ClientID)
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResp{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return tokenResp{}, err
	}
	defer resp.Body.Close()
	var t tokenResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return tokenResp{}, fmt.Errorf("Token-Antwort %d unlesbar", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || t.Access == "" {
		return tokenResp{}, fmt.Errorf("Keycloak antwortet %d: %s %s", resp.StatusCode, t.Error, t.ErrorDescr)
	}
	return t, nil
}

// textual: JSON und Text gehen an den Agenten, alles andere nicht. Ohne Angabe gilt die Antwort als Text.
func textual(ct string) bool {
	if ct == "" {
		return true
	}
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	return strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json") ||
		mt == "application/problem+json" || mt == "application/xml"
}

// secretKeys sind Teile von JSON-Schlüsseln, deren Werte nie beim Agenten ankommen (Review K2):
// Passwort des Connectors, API- und Edge-Schlüssel, Tokens.
var secretKeys = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "edge_key", "private_key", "credential"}

const redacted = "[geschwärzt vom Orchestrator]"

// redact ersetzt in einer JSON-Antwort die Werte geheimer Schlüssel und gibt sie kompakt zurück.
func redact(raw []byte) []byte {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return raw
	}
	v = redactValue(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return raw
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isSecretKey(k) && val != nil && val != "" {
				x[k] = redacted
				continue
			}
			x[k] = redactValue(val)
		}
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
	}
	return v
}

func isSecretKey(k string) bool {
	lk := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
	for _, s := range secretKeys {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

func dateien(n int) string {
	if n == 1 {
		return "1 Datei"
	}
	return fmt.Sprintf("%d Dateien", n)
}

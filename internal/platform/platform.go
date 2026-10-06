// Package platform talks to the REST API of the Agri-Gaia platform. The token
// lives in the orchestrator alone; the agent only names method, path, query and
// JSON body, and the client builds the URL itself from the configured base.
//
// Login: the orchestrator logs in the configured user via password grant. Without
// token exchange (Exchange=false) this token applies to all chats, via the Keycloak client
// "frontend". With token exchange (own confidential client, e.g. "agw-agent") the
// orchestrator exchanges it per chat under RFC 8693 for a token of its own with a restricted audience:
// sub stays the user, azp names the agent (keycloak-token-austausch.md in the thesis repo).
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

// Config holds the settings from the environment (AGW_PLATFORM_*).
type Config struct {
	APIURL   string // https://api.…  (empty: binding off)
	TokenURL string // …/realms/<realm>/protocol/openid-connect/token
	ClientID string
	// ClientSecret of the confidential client (empty: public client such as "frontend").
	ClientSecret string
	User         string
	Password     string
	// Subject, if set, returns the token of the user who owns the chat (login through
	// the platform, AGW_AUTH_MODE=oidc). It replaces the password grant: User and Password are not needed.
	// With Exchange it is the subject_token of the exchange; without Exchange it goes straight to the API.
	Subject func(ctx context.Context, chatID string) (string, error)
	// Exchange: a token of its own per chat via token exchange (requires ClientSecret).
	Exchange bool
	// Audiences are the audiences of the exchanged token (default: backend, minio; MinIO
	// checks the audience when the backend passes the token on for uploads).
	Audiences []string
	// ChatTokenMaxAge: a chat uses its exchanged token this long, then it is exchanged again
	// (default DefaultChatTokenMaxAge). Every exchange is in the log.
	ChatTokenMaxAge time.Duration
	// OnExchange reports every exchange with the details from the new token (for the log).
	OnExchange func(chatID string, c Claims)
	HTTP       *http.Client
	// MaxResult limits the response body the agent sees (default DefaultMaxResult).
	MaxResult int
}

const (
	DefaultChatTokenMaxAge = 30 * time.Minute
	DefaultMaxResult       = 64 << 10
	maxBody                = 1 << 20 // JSON body from the agent
	maxRead                = 8 << 20 // platform response, before truncation
)

// Request is a call as the agent describes it.
type Request struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
	// Form and Files turn the call into multipart/form-data (uploads of datasets and
	// models). Files names paths in the execution sandbox; they are read by the
	// orchestrator (Uploads), never sent along by the agent.
	Form    map[string][]string `json:"form,omitempty"`
	Files   []File              `json:"files,omitempty"`
	Uploads []Upload            `json:"-"`
	// Digest condenses a successful response in the orchestrator before it is truncated
	// (e.g. the OpenAPI description into the path list); the agent cannot set it.
	Digest func([]byte) ([]byte, error) `json:"-"`
	// Full: do not truncate the response to MaxResult (REST endpoint; truncated JSON would be useless).
	// The upper limit maxRead still applies.
	Full bool `json:"-"`
}

// File is a file uploaded with a call: form field and path in the
// execution sandbox.
type File struct {
	Field string `json:"field"`
	Path  string `json:"path"`
}

// Upload is a file that has been read, ready for the call.
type Upload struct {
	Field  string
	Name   string
	Data   []byte
	SHA256 string
}

const (
	MaxFiles       = 2000      // files per call
	MaxUploadBytes = 512 << 20 // total size of the files per call
	maxFormValue   = 16 << 10
	maxFormTotal   = 256 << 10
)

var fieldRe = regexp.MustCompile(`^[A-Za-z0-9_\-.\[\]]{1,64}$`)

// writingGETs are GET paths that write in the backend (Review K1, checked against the backend code):
// /train/containers/{id}/model creates a model including objects in MinIO, /licenses with
// return_cached=false starts an analysis and, with github_token, sets an environment variable of the
// backend. The HTTP method alone is therefore no indicator of "writes".
var writingGETs = []*regexp.Regexp{
	regexp.MustCompile(`^/train/containers/[^/]+/model$`),
	regexp.MustCompile(`^/licenses/?$`),
}

// Writes: every call other than GET may change something and needs an approval, plus the
// known writing GETs. GET /train/containers incidentally syncs states and removes
// entries of vanished containers; that counts as reading (README, limitations).
func (r Request) Writes() bool {
	if r.Method != http.MethodGet {
		return true
	}
	for _, re := range writingGETs {
		if re.MatchString(r.Path) {
			// /licenses only reads with return_cached (default true) and without github_token.
			if strings.HasPrefix(r.Path, "/licenses") && len(r.Query) == 0 {
				return false
			}
			return true
		}
	}
	return false
}

// String is the short form for the log and the approval: "POST /train/config?x=1".
func (r Request) String() string {
	s := r.Method + " " + r.Path
	if q := r.encodedQuery(); q != "" {
		s += "?" + q
	}
	if len(r.Files) > 0 {
		s += " (multipart, " + fileCount(len(r.Files)) + ")"
	} else if len(r.Form) > 0 {
		s += " (multipart)"
	}
	return s
}

// Describe shows form fields and read files for the approval, in full.
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
		fmt.Fprintf(&b, "\n%s, %d bytes in total:\n", fileCount(len(r.Uploads)), total)
		for _, u := range r.Uploads {
			fmt.Fprintf(&b, "  %s: %s  %d bytes  sha256 %s\n", u.Field, u.Name, len(u.Data), u.SHA256)
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

// Result is the response to the agent.
type Result struct {
	Status     string `json:"status"` // ok | rejected | denied | error
	HTTPStatus int    `json:"http_status,omitempty"`
	Location   string `json:"location,omitempty"`
	Body       string `json:"body,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Message    string `json:"message,omitempty"`
	// Violation: a violation that was only logged and let through (delegation without enforce);
	// the agent does not see it.
	Violation string `json:"-"`
}

// RightsPath is the path under which the orchestrator itself answers the chat's delegated
// rights (tool rights); it never goes to the platform.
const RightsPath = "/_agw/rights"

// Text is the rendering for MCP and CLI.
func (r Result) Text() string {
	var b strings.Builder
	switch r.Status {
	case "rejected":
		fmt.Fprintf(&b, "rejected: %s", r.Message)
		return b.String()
	case "denied":
		fmt.Fprintf(&b, "denied by the authorization service: %s (platform_rights or agw-platform rights shows which rights apply)", r.Message)
		return b.String()
	case "error":
		fmt.Fprintf(&b, "error: %s", r.Message)
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
		b.WriteString("\n[… truncated; ask again with skip/limit or a narrower call]")
	}
	return b.String()
}

var (
	ErrNotConfigured = errors.New("platform binding not configured (AGW_PLATFORM_API_URL)")
	pathRe           = regexp.MustCompile(`^/[A-Za-z0-9_\-.:~/ ]*$`) // space: architecture "Mask R-CNN"
	methods          = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	// Blocked areas: /urls returns credentials of integrated services (Fuseki admin,
	// rechtemodell.md in the thesis repo), /service are internal callbacks of Keycloak and the registry,
	// /users reveals the token profile of the orchestrator account, /network returns the password and
	// API key of the dataspace connector (Review K2). The list is a denylist and
	// not complete; redact additionally redacts keys holding secrets in every response.
	blocked = []string{"/urls", "/service", "/users", "/network"}
	// Query limits, so that the approval and the log show the call in full (Review W1).
	maxQueryValue = 1 << 10
	maxQueryTotal = 4 << 10
)

// Normalize checks an agent call and brings it into a canonical form.
func Normalize(r Request) (Request, error) {
	r.Method = strings.ToUpper(strings.TrimSpace(r.Method))
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	if !methods[r.Method] {
		return r, fmt.Errorf("method %q not allowed (GET, POST, PUT, PATCH, DELETE)", r.Method)
	}
	if !pathRe.MatchString(r.Path) {
		return r, fmt.Errorf("path %q invalid: relative to the API, starting with /, without query (that belongs in query)", r.Path)
	}
	if strings.Contains(r.Path, "//") {
		return r, fmt.Errorf("path %q invalid: double slash", r.Path)
	}
	for _, seg := range strings.Split(r.Path, "/") {
		if seg == "." || seg == ".." {
			return r, fmt.Errorf("path %q invalid: . and .. are not allowed", r.Path)
		}
	}
	total := 0
	for k, v := range r.Query {
		if len(v) > maxQueryValue || len(k) > 128 {
			return r, fmt.Errorf("query parameter %q too long (at most %d bytes per value)", k, maxQueryValue)
		}
		total += len(k) + len(v)
	}
	if total > maxQueryTotal {
		return r, fmt.Errorf("query too long (at most %d bytes)", maxQueryTotal)
	}
	if len(r.Form) > 0 || len(r.Files) > 0 {
		if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
			return r, errors.New("form and files only with POST, PUT or PATCH")
		}
		if len(r.Body) > 0 {
			return r, errors.New("either JSON body or form, not both")
		}
		if len(r.Files) > MaxFiles {
			return r, fmt.Errorf("at most %d files per call", MaxFiles)
		}
		ftotal := 0
		for k, vs := range r.Form {
			if !fieldRe.MatchString(k) {
				return r, fmt.Errorf("form field %q invalid", k)
			}
			for _, v := range vs {
				if len(v) > maxFormValue || strings.ContainsRune(v, 0) {
					return r, fmt.Errorf("form field %q: value too long or containing NUL", k)
				}
				ftotal += len(v)
			}
		}
		if ftotal > maxFormTotal {
			return r, errors.New("form too large")
		}
		for _, f := range r.Files {
			if !fieldRe.MatchString(f.Field) {
				return r, fmt.Errorf("file field %q invalid", f.Field)
			}
			if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || strings.ContainsRune(f.Path, 0) || f.Path == "/" {
				return r, fmt.Errorf("file path %q invalid: give it absolute and without . or ..", f.Path)
			}
		}
	}
	lp := strings.ToLower(r.Path)
	for _, b := range blocked {
		if lp == b || strings.HasPrefix(lp, b+"/") {
			return r, fmt.Errorf("path %q is blocked for the agent", r.Path)
		}
	}
	if len(r.Body) > 0 {
		if r.Method == http.MethodGet {
			return r, errors.New("GET takes no body")
		}
		if len(r.Body) > maxBody {
			return r, fmt.Errorf("body larger than %d KiB", maxBody>>10)
		}
		if !json.Valid(r.Body) {
			return r, errors.New("body is not valid JSON")
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

// Client holds the user's token and, with token exchange, an exchanged token per chat.
type Client struct {
	cfg  Config
	base *url.URL

	mu      sync.Mutex
	access  string
	refresh string
	expires time.Time
	chats   map[string]chatToken
	xmu     sync.Mutex // one exchange at a time, so that concurrent calls of a chat do not exchange twice

	// exchanged: outcome of the last token exchange per chat (status view, in memory only).
	exchanged map[string]ExchangeStatus
	probeMu   sync.Mutex // one probe at a time
	probe     Probe      // last probe of the API (cached for ProbeMaxAge)
}

type chatToken struct {
	access  string
	expires time.Time // the earlier of the token's expiry and ChatTokenMaxAge
}

// Claims are the details of a token that the log records (read unverified; the token
// comes from our own request to Keycloak).
type Claims struct {
	Sub      string    `json:"sub"`
	Username string    `json:"preferred_username"`
	Azp      string    `json:"azp"`
	Aud      []string  `json:"-"`
	Act      any       `json:"act,omitempty"`
	Exp      time.Time `json:"-"`
	JTI      string    `json:"jti"`
}

// String: "sub=test azp=agw-agent aud=backend,minio exp=…" for the log.
func (c Claims) String() string {
	s := fmt.Sprintf("user %s, azp=%s, aud=%s, valid until %s", c.Username, c.Azp, strings.Join(c.Aud, ","), c.Exp.Format(time.RFC3339))
	if c.Act != nil {
		b, _ := json.Marshal(c.Act)
		s += ", act=" + string(b)
	} else {
		s += ", without act (impersonation)"
	}
	return s
}

// ParseClaims reads the payload of a JWT without verifying the signature.
func ParseClaims(tok string) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("not a JWT")
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

// New returns nil if no API URL is configured (binding off).
func New(cfg Config) (*Client, error) {
	if cfg.APIURL == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimRight(cfg.APIURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("AGW_PLATFORM_API_URL invalid: %q", cfg.APIURL)
	}
	if cfg.Subject == nil && (cfg.TokenURL == "" || cfg.User == "" || cfg.Password == "") {
		return nil, errors.New("AGW_PLATFORM_TOKEN_URL, AGW_PLATFORM_USER and AGW_PLATFORM_PASSWORD must be set")
	}
	if cfg.Subject != nil && cfg.Exchange && cfg.TokenURL == "" {
		return nil, errors.New("token exchange requires AGW_PLATFORM_TOKEN_URL")
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "frontend"
	}
	if cfg.Exchange && cfg.ClientSecret == "" {
		return nil, errors.New("token exchange (AGW_PLATFORM_TOKEN_EXCHANGE) requires AGW_PLATFORM_CLIENT_SECRET of a confidential client")
	}
	if len(cfg.Audiences) == 0 {
		cfg.Audiences = []string{"backend", "minio"}
	}
	if cfg.ChatTokenMaxAge <= 0 {
		cfg.ChatTokenMaxAge = DefaultChatTokenMaxAge
	}
	if cfg.HTTP == nil {
		// No proxy from the environment; neither the token call nor the API call follows redirects,
		// otherwise the password (307/308) or the token would go to a foreign destination (Review W4).
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		cfg.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: tr}
	}
	hc := *cfg.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &hc
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		slog.Warn("platform API without TLS: token is sent in plain text", "api", cfg.APIURL)
	}
	if cfg.MaxResult <= 0 {
		cfg.MaxResult = DefaultMaxResult
	}
	return &Client{cfg: cfg, base: u, chats: map[string]chatToken{}, exchanged: map[string]ExchangeStatus{}}, nil
}

// Do performs a call. The check (Normalize) runs here once more, so that no caller
// can bypass it (Review W2). A platform error is not a Go error but a Result with
// status "error".
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
		// Downloads (ZIP, models) would only be noise in the context (Review W3).
		res.Body = fmt.Sprintf("[binary response, %d bytes, %s; not passed on]", len(raw), ct)
		if resp.StatusCode >= 400 {
			res.Status, res.Message = "error", fmt.Sprintf("platform responds %d", resp.StatusCode)
		}
		return res, nil
	}
	if len(raw) > maxRead {
		// Truncated JSON could no longer be redacted; then none of it is passed on (Review 5, W4).
		res.Truncated = true
		res.Body = fmt.Sprintf("[response larger than %d MiB; not passed on because it cannot be redacted once truncated. Narrow it down with skip/limit]", maxRead>>20)
		if resp.StatusCode >= 400 {
			res.Status, res.Message = "error", fmt.Sprintf("platform responds %d", resp.StatusCode)
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
		res.Status, res.Message = "error", fmt.Sprintf("platform responds %d", resp.StatusCode)
	}
	return res, nil
}

// relLocation shortens a platform Location to its path, so that the agent can
// use it directly as path again.
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
		return nil, fmt.Errorf("login to the platform: %w", err)
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
			return nil, errors.New("the call's files have not been read")
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
		for _, up := range r.Uploads { // order as given: the annotation is the last one
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

// token returns a valid access token: cached, renewed via refresh_token shortly before
// expiry, and logged in again on force or a failed renewal.
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
		if ctx.Err() == nil { // a cancelled call does not discard everyone's token (Review M1)
			c.access, c.refresh = "", ""
		}
		return "", err
	}
	return c.access, nil
}

// Forget discards a chat's exchanged token (when idling; resuming exchanges again).
// It cannot be revoked: the backend only checks tokens by their signature.
func (c *Client) Forget(chatID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.chats, chatID)
}

// SetOnExchange sets the per-exchange report (the manager is created after the client).
func (c *Client) SetOnExchange(f func(chatID string, cl Claims)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.OnExchange = f
}

// Exchanging reports whether the client exchanges per chat.
func (c *Client) Exchanging() bool { return c != nil && c.cfg.Exchange }

// tokenFor returns the token for a call: without token exchange the user's, otherwise the
// chat's exchanged token (cached until ChatTokenMaxAge or shortly before expiry).
func (c *Client) tokenFor(ctx context.Context, chatID string, force bool) (string, error) {
	if !c.cfg.Exchange {
		return c.userToken(ctx, chatID, force)
	}
	if chatID == "" {
		return "", errors.New("token exchange needs a chat")
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
		return ct2.access, nil // exchanged by another call in the meantime
	}
	user, err := c.userToken(ctx, chatID, force)
	if err != nil {
		if ctx.Err() == nil {
			c.noteExchange(chatID, err) // no subject token, so no exchange either (e.g. the user's login expired)
		}
		return "", err
	}
	tok, err := c.exchange(ctx, user)
	if err != nil && !force && c.cfg.Subject == nil {
		// The user token may have expired on the server (session ended): log in again once.
		// With Subject there is no password; the token comes fresh from the user's session.
		if user, err = c.token(ctx, true); err == nil {
			tok, err = c.exchange(ctx, user)
		}
	}
	if err != nil {
		c.noteExchange(chatID, err)
		return "", fmt.Errorf("token exchange: %w", err)
	}
	c.noteExchange(chatID, nil)
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

// userToken: the token of the user who owns the chat (Subject), otherwise that of the configured account.
func (c *Client) userToken(ctx context.Context, chatID string, force bool) (string, error) {
	if c.cfg.Subject == nil {
		return c.token(ctx, force)
	}
	if chatID == "" {
		return "", errors.New("user login needs a chat")
	}
	return c.cfg.Subject(ctx, chatID)
}

// exchange exchanges the user token under RFC 8693 for one with the configured audiences.
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

// post sends a form to the token endpoint (with client ID and, if set, secret).
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
		return tokenResp{}, fmt.Errorf("token response %d unreadable", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || t.Access == "" {
		return tokenResp{}, fmt.Errorf("Keycloak responds %d: %s %s", resp.StatusCode, t.Error, t.ErrorDescr)
	}
	return t, nil
}

// textual: JSON and text go to the agent, nothing else does. Without a content type the response counts as text.
func textual(ct string) bool {
	if ct == "" {
		return true
	}
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	return strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json") ||
		mt == "application/problem+json" || mt == "application/xml"
}

// secretKeys are parts of JSON keys whose values never reach the agent (Review K2):
// the connector's password, API and edge keys, tokens.
var secretKeys = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "edge_key", "private_key", "credential"}

const redacted = "[redacted by the orchestrator]"

// redact replaces the values of secret keys in a JSON response and returns it compacted.
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

func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

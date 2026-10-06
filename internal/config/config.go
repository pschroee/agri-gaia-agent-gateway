// Package config reads the orchestrator's environment and the model catalog.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Currency   string  `json:"currency"`
	Note       string  `json:"note,omitempty"`
	Source     string  `json:"source,omitempty"`    // URL of the price source
	Retrieved  string  `json:"retrieved,omitempty"` // retrieval date (ISO)
}

type Model struct {
	ID            string   `json:"id"`
	PiBuiltin     bool     `json:"pi_builtin"` // pi knows the model; do not redefine it
	Note          string   `json:"note,omitempty"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	ContextWindow int      `json:"context_window"`
	MaxTokens     int      `json:"max_tokens"`
	Pricing       *Pricing `json:"pricing"`
}

type Provider struct {
	ID        string          `json:"id"`
	Upstream  string          `json:"upstream"`
	API       string          `json:"api"`
	APIKeyEnv string          `json:"api_key_env"`
	Compat    json.RawMessage `json:"compat,omitempty"`
	// TitleRequest: extra fields the orchestrator's title call (package titler) sends.
	TitleRequest json.RawMessage `json:"title_request,omitempty"`
	Tariff       *Tariff         `json:"tariff,omitempty"`
	Models       []Model         `json:"models"`
}

// APIKey reads the key from the orchestrator's environment.
func (p Provider) APIKey() string { return os.Getenv(p.APIKeyEnv) }

type Catalog struct {
	Default   string     `json:"default"`
	Providers []Provider `json:"providers"`

	regMu     sync.RWMutex
	registry  map[string]RegistryModel // "provider/model" from get_available_models
	piVersion string
}

// RegistryModel is a model from pi's own registry (get_available_models).
type RegistryModel struct {
	Provider      string       `json:"provider"`
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	ContextWindow int          `json:"contextWindow"`
	MaxTokens     int          `json:"maxTokens"`
	Cost          RegistryCost `json:"cost"`
}

type RegistryCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// SetRegistry takes over pi's registry. Only models in the catalog count.
func (c *Catalog) SetRegistry(piVersion string, models []RegistryModel) {
	m := map[string]RegistryModel{}
	for _, r := range models {
		m[r.Provider+"/"+r.ID] = r
	}
	c.regMu.Lock()
	c.registry, c.piVersion = m, piVersion
	c.regMu.Unlock()
}

func (c *Catalog) HasRegistry() bool {
	c.regMu.RLock()
	defer c.regMu.RUnlock()
	return c.registry != nil
}

// EffectivePricing: the catalog's own prices take precedence over pi's registry.
func (c *Catalog) EffectivePricing(id string) *Pricing {
	_, m, ok := c.Lookup(id)
	if !ok {
		return nil
	}
	if m.Pricing != nil {
		p := *m.Pricing
		return &p
	}
	c.regMu.RLock()
	r, ok := c.registry[id]
	ver := c.piVersion
	c.regMu.RUnlock()
	if !ok {
		return nil
	}
	note := "from the model registry of pi " + ver
	if m.Note != "" {
		note = m.Note + "; " + note
	}
	p := &Pricing{Input: r.Cost.Input, Output: r.Cost.Output, CacheRead: r.Cost.CacheRead, CacheWrite: r.Cost.CacheWrite, Currency: "USD", Note: note}
	// The provider's pricing page serves as the source, if the tariff names it.
	if prov, _, ok := c.Lookup(id); ok && prov.Tariff != nil {
		p.Source, p.Retrieved = prov.Tariff.Source, prov.Tariff.Retrieved
	}
	return p
}

// ModelInfo is the API's view of a model.
type ModelInfo struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Name     string   `json:"name"`
	Default  bool     `json:"default"`
	Pricing  *Pricing `json:"pricing,omitempty"`
	Tariff   *Tariff  `json:"tariff,omitempty"`
	PeakNow  *bool    `json:"peak_now,omitempty"` // is the peak tariff in effect right now?
	// ContextWindow in tokens (0 = unknown); for the check when switching models.
	ContextWindow int64 `json:"context_window,omitempty"`
}

func LoadCatalog(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCatalog(b)
}

func ParseCatalog(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("model catalog: %w", err)
	}
	for i := range c.Providers {
		if t := c.Providers[i].Tariff; t != nil {
			if err := t.validate(); err != nil {
				return nil, fmt.Errorf("model catalog, tariff of %s: %w", c.Providers[i].ID, err)
			}
		}
		for j := range c.Providers[i].Models {
			if pr := c.Providers[i].Models[j].Pricing; pr != nil && pr.Currency == "" {
				pr.Currency = "USD"
			}
		}
	}
	if _, _, ok := c.Lookup(c.Default); !ok {
		return nil, fmt.Errorf("model catalog: default model %q is not in the catalog", c.Default)
	}
	return &c, nil
}

// Lookup takes an identifier of the form "provider/model".
func (c *Catalog) Lookup(id string) (Provider, Model, bool) {
	prov, model, ok := strings.Cut(id, "/")
	if !ok {
		return Provider{}, Model{}, false
	}
	for _, p := range c.Providers {
		if p.ID != prov {
			continue
		}
		for _, m := range p.Models {
			if m.ID == model {
				return p, m, true
			}
		}
	}
	return Provider{}, Model{}, false
}

// ContextWindow: a model's context window in tokens (catalog, otherwise pi's registry; 0 = unknown).
func (c *Catalog) ContextWindow(id string) int64 {
	if _, m, ok := c.Lookup(id); ok && m.ContextWindow > 0 {
		return int64(m.ContextWindow)
	}
	c.regMu.RLock()
	defer c.regMu.RUnlock()
	return int64(c.registry[id].ContextWindow)
}

func (c *Catalog) Provider(id string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

func (c *Catalog) Models() []ModelInfo {
	var out []ModelInfo
	for _, p := range c.Providers {
		for _, m := range p.Models {
			id := p.ID + "/" + m.ID
			name := m.Name
			c.regMu.RLock()
			if r, ok := c.registry[id]; ok && name == "" {
				name = r.Name
			}
			c.regMu.RUnlock()
			if name == "" {
				name = m.ID
			}
			mi := ModelInfo{ID: id, Provider: p.ID, Model: m.ID, Name: name, Default: id == c.Default, Pricing: c.EffectivePricing(id), ContextWindow: c.ContextWindow(id)}
			if p.Tariff != nil && len(p.Tariff.PeakWindowsUTC) > 0 {
				mi.Tariff = p.Tariff
				now := p.Tariff.IsPeak(time.Now())
				mi.PeakNow = &now
			}
			out = append(out, mi)
		}
	}
	return out
}

// PiModelsJSON produces the models.json for pi in the sandbox. Every provider
// points to the orchestrator's proxy; the key is a placeholder.
func (c *Catalog) PiModelsJSON(proxyBase string) ([]byte, error) {
	type piCost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
	}
	type piModel struct {
		ID            string  `json:"id"`
		Name          string  `json:"name,omitempty"`
		Reasoning     bool    `json:"reasoning"`
		ContextWindow int     `json:"contextWindow,omitempty"`
		MaxTokens     int     `json:"maxTokens,omitempty"`
		Cost          *piCost `json:"cost,omitempty"`
	}
	type piProvider struct {
		BaseURL string          `json:"baseUrl"`
		API     string          `json:"api"`
		APIKey  string          `json:"apiKey"`
		Compat  json.RawMessage `json:"compat,omitempty"`
		Models  []piModel       `json:"models,omitempty"`
	}
	doc := struct {
		Providers map[string]piProvider `json:"providers"`
	}{Providers: map[string]piProvider{}}
	base := strings.TrimRight(proxyBase, "/")
	for _, p := range c.Providers {
		pp := piProvider{BaseURL: base + "/llm/" + p.ID, API: p.API, APIKey: "agw-proxy", Compat: p.Compat}
		for _, m := range p.Models {
			if m.PiBuiltin {
				continue // pi keeps model, compat and prices from its registry
			}
			pm := piModel{ID: m.ID, Name: m.Name, Reasoning: m.Reasoning, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens}
			if m.Pricing != nil {
				pm.Cost = &piCost{m.Pricing.Input, m.Pricing.Output, m.Pricing.CacheRead, m.Pricing.CacheWrite}
			}
			pp.Models = append(pp.Models, pm)
		}
		doc.Providers[p.ID] = pp
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Env holds the settings from the environment.
type Env struct {
	HTTPAddr             string
	ProxyAddr            string
	WebProxyAddr         string // web proxy for web_search/web_extract from pi's container
	WebProxyURL          string // how pi's container reaches the web proxy
	SearxURL             string // own SearXNG service as seen from the orchestrator (empty: none)
	ProxyBaseURL         string // how the sandbox reaches the proxy
	CatalogPath          string
	DatabaseURL          string
	S3Endpoint           string
	S3AccessKey          string
	S3SecretKey          string
	S3Bucket             string
	Image                string // execution sandbox (E9): tools, Python, Typst …
	PiImage              string // pi's container (E9): no shell, no Python
	SandboxNetwork       string
	EgressNetwork        string
	EgressSubnet         string
	BlockedSubnets       string
	NpmCache             string // container of the npm package cache, empty = none
	PipCache             string // container of the pip package cache, empty = none
	InternetDefault      bool
	AutoCompactDefault   bool
	CompactReserveTokens int
	CompactKeepRecent    int
	SocketVolume         string // named volume holding the socket directories
	SocketRoot           string // mount point of this volume in the orchestrator
	PoolSizes            map[string]int
	IdleTimeout          time.Duration
	ApprovalTimeout      time.Duration
	ArtifactMaxBytes     int64
	ImageMaxBytes        int64 // display images in responses (AGW_IMAGE_MAX_MB)
	WorkspaceMaxBytes    int64 // backup of /workspace per chat (AGW_WORKSPACE_MAX_MB; 0 = off → -1)
	SandboxMemoryMB      int64 // pi's container (pi and up to four subagents)
	SandboxCPUs          float64
	SandboxPids          int64
	ExecMemoryMB         int64 // execution sandbox
	ExecCPUs             float64
	ExecPids             int64
	DefaultModel         string
	TitleModel           string // model for chat titles ("provider/model"; empty: the chat's, "off": none)
	APIToken             string
	// MaxSubagents: at most this many subagents run at the same time per chat (AGW_MAX_SUBAGENTS,
	// default 5); fixed for the service, not a setting per chat.
	MaxSubagents int
	AllowedHosts []string
	// Background tasks: at most BgMax at a time per slot (AGW_BG_MAX), at most
	// BgWakesPerHour wake-ups per chat and hour (AGW_BG_WAKES_PER_HOUR, 0 = never); running tasks
	// keep the chat awake until BgKeepAlive after the last activity (AGW_BG_KEEPALIVE).
	BgMax          int
	BgWakesPerHour int
	BgKeepAlive    time.Duration
	// AutoTurnsMax: at most this many consecutive turns without the user per chat
	// (AGW_AUTO_TURNS_MAX, default 5, 0 = none; Review 3, H2).
	AutoTurnsMax int
	// Platform binding (direct, fixed account via password grant). Off without PlatformAPIURL.
	PlatformAPIURL   string
	PlatformTokenURL string
	PlatformClientID string
	PlatformUser     string
	PlatformPassword string
	// Token exchange per chat (RFC 8693) via a confidential client, e.g. agw-agent.
	PlatformClientSecret string
	PlatformExchange     bool
	PlatformAudiences    []string
	// Login to the API: AuthMode "token" (AGW_API_TOKEN, default) or "oidc" (login through the
	// platform's Keycloak, authorization code with PKCE; chats then belong to the user).
	AuthMode         string
	OIDCIssuer       string // https://keycloak.<base>/realms/<realm>
	OIDCClientID     string // default agw-agent
	OIDCClientSecret string // default: AGW_PLATFORM_CLIENT_SECRET
	PublicURL        string // https://app.<base>/agent or https://agent.<base>; redirect URI = PublicURL + "/oidc/callback"
	// BasePath: path of PublicURL ("" or e.g. "/agent"); the UI lives under BasePath + "/". The proxy
	// strips it, but addresses for the browser still need it.
	BasePath string
	// FrameAncestors: origins allowed to embed the UI (empty: frame-ancestors 'none').
	FrameAncestors []string
}

// DefaultMaxSubagents: subagents that may run at the same time per chat unless AGW_MAX_SUBAGENTS says
// otherwise (issue #24: fixed, no setting per chat).
const DefaultMaxSubagents = 5

// Login modes of the API (AGW_AUTH_MODE).
const (
	AuthToken = "token"
	AuthOIDC  = "oidc"
)

func FromEnv() Env {
	return Env{
		HTTPAddr:             ":" + str("AGW_HTTP_PORT", "18480"),
		ProxyAddr:            ":" + str("AGW_PROXY_PORT", "18481"),
		WebProxyAddr:         ":" + str("AGW_WEB_PROXY_PORT", "18486"),
		WebProxyURL:          str("AGW_WEB_PROXY_URL", "http://orchestrator:18486"),
		SearxURL:             str("AGW_SEARXNG_URL", "http://searxng:8080"),
		ProxyBaseURL:         str("AGW_PROXY_BASE_URL", "http://orchestrator:18481"),
		CatalogPath:          str("AGW_CATALOG", "/etc/agw/models.json"),
		DatabaseURL:          str("AGW_DATABASE_URL", ""),
		S3Endpoint:           str("AGW_S3_ENDPOINT", "rustfs:9000"),
		S3AccessKey:          str("RUSTFS_ACCESS_KEY", ""),
		S3SecretKey:          str("RUSTFS_SECRET_KEY", ""),
		S3Bucket:             str("AGW_S3_BUCKET", "agw-artifacts"),
		Image:                str("AGW_IMAGE", "agwpoc/agw-basis:dev"),
		PiImage:              str("AGW_PI_IMAGE", "agwpoc/agw-pi:dev"),
		SandboxNetwork:       str("AGW_SANDBOX_NETWORK", "agwpoc_sandbox"),
		EgressNetwork:        str("AGW_EGRESS_NETWORK", "agwpoc_egress"),
		EgressSubnet:         str("AGW_EGRESS_SUBNET", "10.231.20.0/24"), // never Docker's 172.x (would otherwise hide VPN destinations)
		BlockedSubnets:       str("AGW_BLOCKED_SUBNETS", ""),
		NpmCache:             str("AGW_NPM_CACHE", ""),
		PipCache:             str("AGW_PIP_CACHE", ""),
		InternetDefault:      str("AGW_INTERNET_DEFAULT", "false") == "true",
		AutoCompactDefault:   str("AGW_AUTO_COMPACT_DEFAULT", "true") == "true",
		CompactReserveTokens: num("AGW_COMPACT_RESERVE_TOKENS", 16384),
		CompactKeepRecent:    num("AGW_COMPACT_KEEP_RECENT_TOKENS", 20000),
		SocketVolume:         str("AGW_SOCKET_VOLUME", "agwpoc_sockets"),
		SocketRoot:           str("AGW_SOCKET_ROOT", "/run/agw"),
		PoolSizes: map[string]int{
			"cli":  num("AGW_POOL_SIZE_CLI", 1),
			"mcp":  num("AGW_POOL_SIZE_MCP", 1),
			"both": num("AGW_POOL_SIZE_BOTH", 0),
			"api":  num("AGW_POOL_SIZE_API", 0),
		},
		IdleTimeout:          dur("AGW_IDLE_TIMEOUT", 10*time.Minute),
		ApprovalTimeout:      dur("AGW_APPROVAL_TIMEOUT", 10*time.Minute),
		ArtifactMaxBytes:     int64(num("AGW_ARTIFACT_MAX_MB", 50)) << 20,
		ImageMaxBytes:        int64(num("AGW_IMAGE_MAX_MB", 10)) << 20,
		WorkspaceMaxBytes:    workspaceMax(num("AGW_WORKSPACE_MAX_MB", 200)),
		SandboxMemoryMB:      int64(num("AGW_SANDBOX_MEMORY_MB", 2048)),
		SandboxCPUs:          flt("AGW_SANDBOX_CPUS", 1),
		SandboxPids:          int64(num("AGW_SANDBOX_PIDS", 256)),
		ExecMemoryMB:         int64(num("AGW_EXEC_MEMORY_MB", 2048)),
		ExecCPUs:             flt("AGW_EXEC_CPUS", 1),
		ExecPids:             int64(num("AGW_EXEC_PIDS", 256)),
		DefaultModel:         str("AGW_DEFAULT_MODEL", ""),
		TitleModel:           str("AGW_TITLE_MODEL", ""),
		APIToken:             str("AGW_API_TOKEN", ""),
		MaxSubagents:         max(0, num("AGW_MAX_SUBAGENTS", DefaultMaxSubagents)),
		AllowedHosts:         strings.Split(str("AGW_ALLOWED_HOSTS", "127.0.0.1:18480,localhost:18480"), ","),
		BgMax:                max(1, num("AGW_BG_MAX", 5)),
		BgWakesPerHour:       wakes(num("AGW_BG_WAKES_PER_HOUR", 10)),
		BgKeepAlive:          keepAlive(dur("AGW_BG_KEEPALIVE", time.Hour)),
		AutoTurnsMax:         wakes(num("AGW_AUTO_TURNS_MAX", 5)),
		PlatformAPIURL:       str("AGW_PLATFORM_API_URL", ""),
		PlatformTokenURL:     str("AGW_PLATFORM_TOKEN_URL", ""),
		PlatformClientID:     str("AGW_PLATFORM_CLIENT_ID", "frontend"),
		PlatformUser:         str("AGW_PLATFORM_USER", ""),
		PlatformPassword:     str("AGW_PLATFORM_PASSWORD", ""),
		PlatformClientSecret: str("AGW_PLATFORM_CLIENT_SECRET", ""),
		PlatformExchange:     str("AGW_PLATFORM_TOKEN_EXCHANGE", "false") == "true",
		PlatformAudiences:    strings.Split(str("AGW_PLATFORM_AUDIENCES", "backend,minio"), ","),
		AuthMode:             strings.ToLower(strings.TrimSpace(str("AGW_AUTH_MODE", AuthToken))),
		OIDCIssuer:           strings.TrimRight(str("AGW_OIDC_ISSUER", ""), "/"),
		OIDCClientID:         str("AGW_OIDC_CLIENT_ID", "agw-agent"),
		OIDCClientSecret:     str("AGW_OIDC_CLIENT_SECRET", str("AGW_PLATFORM_CLIENT_SECRET", "")),
		PublicURL:            strings.TrimRight(str("AGW_PUBLIC_URL", ""), "/"),
		BasePath:             orEmpty(BasePath(strings.TrimRight(str("AGW_PUBLIC_URL", ""), "/"))),
		FrameAncestors:       strings.Fields(str("AGW_FRAME_ANCESTORS", "")),
	}
}

// orEmpty: an invalid value falls back to ""; CheckAuth reports the error.
func orEmpty(s string, err error) string {
	if err != nil {
		return ""
	}
	return s
}

// BasePath returns the path of AGW_PUBLIC_URL without a trailing slash: "" for https://host,
// "/agent" for https://host/agent/. A proxy strips it before the orchestrator; addresses for the
// browser (redirects, cookies, login links) still need it.
func BasePath(publicURL string) (string, error) {
	if publicURL == "" {
		return "", nil
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", fmt.Errorf("AGW_PUBLIC_URL invalid: %q", publicURL)
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		return "", nil
	}
	ok := path.Clean(p) == p && u.RawPath == ""
	for _, c := range p {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/-._~", c)) {
			ok = false
		}
	}
	if !ok {
		return "", fmt.Errorf("AGW_PUBLIC_URL: path %q invalid (allowed e.g. /agent)", u.Path)
	}
	return p, nil
}

// PublicHost returns the host of AGW_PUBLIC_URL (with port, without path), "" if unset. The UI is
// opened at this address, so the host belongs to the allowed Host headers.
func (e Env) PublicHost() string {
	if u, err := url.Parse(e.PublicURL); err == nil {
		return u.Host
	}
	return ""
}

// CheckAuth checks the settings of the chosen login mode.
func (e Env) CheckAuth() error {
	if _, err := BasePath(e.PublicURL); err != nil {
		return err
	}
	switch e.AuthMode {
	case AuthToken:
		if len(e.APIToken) < 32 {
			return errors.New("AGW_API_TOKEN is missing or shorter than 32 characters (./dev.sh init creates it)")
		}
	case AuthOIDC:
		var missing []string
		for k, v := range map[string]string{"AGW_OIDC_ISSUER": e.OIDCIssuer, "AGW_OIDC_CLIENT_ID": e.OIDCClientID,
			"AGW_OIDC_CLIENT_SECRET (or AGW_PLATFORM_CLIENT_SECRET)": e.OIDCClientSecret, "AGW_PUBLIC_URL": e.PublicURL} {
			if v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("AGW_AUTH_MODE=oidc requires %s", strings.Join(missing, ", "))
		}
		for _, o := range e.FrameAncestors {
			if !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://") && o != "'self'" {
				return fmt.Errorf("AGW_FRAME_ANCESTORS: %q is not an origin (https://…)", o)
			}
			if strings.ContainsAny(o, ";,") {
				return fmt.Errorf("AGW_FRAME_ANCESTORS: %q contains ; or ,", o)
			}
		}
	default:
		return fmt.Errorf("AGW_AUTH_MODE must be token or oidc, not %q", e.AuthMode)
	}
	return nil
}

// keepAlive: AGW_BG_KEEPALIVE=0 means "do not postpone idling" (-1 for the manager).
func keepAlive(d time.Duration) time.Duration {
	if d <= 0 {
		return -1
	}
	return d
}

// wakes: AGW_BG_WAKES_PER_HOUR=0 (likewise AGW_AUTO_TURNS_MAX=0) means "never wake" (-1 for the
// manager, whose 0 is the default).
func wakes(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

// workspaceMax: AGW_WORKSPACE_MAX_MB=0 (or negative) turns off the backup of the
// workspace (-1).
func workspaceMax(mb int) int64 {
	if mb <= 0 {
		return -1
	}
	return int64(mb) << 20
}

func str(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func num(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func flt(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

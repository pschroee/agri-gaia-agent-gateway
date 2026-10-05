// Package config liest die Umgebung des Orchestrators und den Modellkatalog.
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
	Source     string  `json:"source,omitempty"`    // URL der Preisangabe
	Retrieved  string  `json:"retrieved,omitempty"` // Abrufdatum (ISO)
}

type Model struct {
	ID            string   `json:"id"`
	PiBuiltin     bool     `json:"pi_builtin"` // pi kennt das Modell; nicht neu definieren
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
	// TitleRequest: Felder, die der Titel-Aufruf des Orchestrators (Paket titler) zusätzlich sendet.
	TitleRequest json.RawMessage `json:"title_request,omitempty"`
	Tariff       *Tariff         `json:"tariff,omitempty"`
	Models       []Model         `json:"models"`
}

// APIKey liest den Schlüssel aus der Umgebung des Orchestrators.
func (p Provider) APIKey() string { return os.Getenv(p.APIKeyEnv) }

type Catalog struct {
	Default   string     `json:"default"`
	Providers []Provider `json:"providers"`

	regMu     sync.RWMutex
	registry  map[string]RegistryModel // "anbieter/modell" aus get_available_models
	piVersion string
}

// RegistryModel ist ein Modell aus pis eigenem Register (get_available_models).
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

// SetRegistry übernimmt pis Register. Nur Modelle des Katalogs zählen.
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

// EffectivePricing: eigene Preise des Katalogs vor pis Register.
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
	note := "aus dem Modellregister von pi " + ver
	if m.Note != "" {
		note = m.Note + "; " + note
	}
	p := &Pricing{Input: r.Cost.Input, Output: r.Cost.Output, CacheRead: r.Cost.CacheRead, CacheWrite: r.Cost.CacheWrite, Currency: "USD", Note: note}
	// Als Beleg dient die Preisseite des Anbieters, soweit der Tarif sie nennt.
	if prov, _, ok := c.Lookup(id); ok && prov.Tariff != nil {
		p.Source, p.Retrieved = prov.Tariff.Source, prov.Tariff.Retrieved
	}
	return p
}

// ModelInfo ist die Sicht der API auf ein Modell.
type ModelInfo struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Name     string   `json:"name"`
	Default  bool     `json:"default"`
	Pricing  *Pricing `json:"pricing,omitempty"`
	Tariff   *Tariff  `json:"tariff,omitempty"`
	PeakNow  *bool    `json:"peak_now,omitempty"` // gilt gerade der Spitzentarif?
	// ContextWindow in Tokens (0 = unbekannt); für die Prüfung beim Modellwechsel.
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
		return nil, fmt.Errorf("Modellkatalog: %w", err)
	}
	for i := range c.Providers {
		if t := c.Providers[i].Tariff; t != nil {
			if err := t.validate(); err != nil {
				return nil, fmt.Errorf("Modellkatalog, Tarif von %s: %w", c.Providers[i].ID, err)
			}
		}
		for j := range c.Providers[i].Models {
			if pr := c.Providers[i].Models[j].Pricing; pr != nil && pr.Currency == "" {
				pr.Currency = "USD"
			}
		}
	}
	if _, _, ok := c.Lookup(c.Default); !ok {
		return nil, fmt.Errorf("Modellkatalog: Standardmodell %q nicht im Katalog", c.Default)
	}
	return &c, nil
}

// Lookup nimmt eine Kennung der Form "anbieter/modell".
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

// ContextWindow: Kontextfenster eines Modells in Tokens (Katalog, sonst pis Register; 0 = unbekannt).
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

// PiModelsJSON erzeugt die models.json für pi in der Sandbox. Jeder Anbieter
// zeigt auf den Proxy des Orchestrators; der Schlüssel ist ein Platzhalter.
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
				continue // pi behält Modell, compat und Preise aus seinem Register
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

// Env sind die Einstellungen aus der Umgebung.
type Env struct {
	HTTPAddr             string
	ProxyAddr            string
	WebProxyAddr         string // Web-Proxy für web_search/web_extract aus dem Container von pi
	WebProxyURL          string // wie der Container von pi den Web-Proxy erreicht
	SearxURL             string // eigener SearXNG-Dienst aus Sicht des Orchestrators (leer: keiner)
	ProxyBaseURL         string // wie die Sandbox den Proxy erreicht
	CatalogPath          string
	DatabaseURL          string
	S3Endpoint           string
	S3AccessKey          string
	S3SecretKey          string
	S3Bucket             string
	Image                string // Ausführungs-Sandbox (E9): Werkzeuge, Python, Typst …
	PiImage              string // Container von pi (E9): ohne Shell, ohne Python
	SandboxNetwork       string
	EgressNetwork        string
	EgressSubnet         string
	BlockedSubnets       string
	NpmCache             string // Container des npm-Zwischenspeichers, leer = keiner
	PipCache             string // Container des pip-Zwischenspeichers, leer = keiner
	InternetDefault      bool
	AutoCompactDefault   bool
	CompactReserveTokens int
	CompactKeepRecent    int
	SocketVolume         string // benanntes Volume mit den Socket-Verzeichnissen
	SocketRoot           string // Einhängepunkt dieses Volumes im Orchestrator
	PoolSizes            map[string]int
	IdleTimeout          time.Duration
	ApprovalTimeout      time.Duration
	ArtifactMaxBytes     int64
	ImageMaxBytes        int64 // Anzeige-Bilder in Antworten (AGW_IMAGE_MAX_MB)
	WorkspaceMaxBytes    int64 // Sicherung von /workspace je Chat (AGW_WORKSPACE_MAX_MB; 0 = aus → -1)
	SandboxMemoryMB      int64 // Container von pi (pi und bis zu vier Subagenten)
	SandboxCPUs          float64
	SandboxPids          int64
	ExecMemoryMB         int64 // Ausführungs-Sandbox
	ExecCPUs             float64
	ExecPids             int64
	DefaultModel         string
	TitleModel           string // Modell für Chattitel ("anbieter/modell"; leer: das des Chats, "off": keine)
	APIToken             string
	MaxSubagentsDefault  int
	MaxSubagentsLimit    int
	AllowedHosts         []string
	// Hintergrundaufgaben: höchstens BgMax gleichzeitig je Platz (AGW_BG_MAX), höchstens
	// BgWakesPerHour Weckrufe je Chat und Stunde (AGW_BG_WAKES_PER_HOUR, 0 = nie), laufende Aufgaben
	// halten den Chat bis BgKeepAlive nach der letzten Aktivität wach (AGW_BG_KEEPALIVE).
	BgMax          int
	BgWakesPerHour int
	BgKeepAlive    time.Duration
	// AutoTurnsMax: höchstens so viele Durchgänge ohne Nutzer hintereinander je Chat
	// (AGW_AUTO_TURNS_MAX, Standard 5, 0 = keiner; Review 3, H2).
	AutoTurnsMax int
	// Plattform-Anbindung (direkt, festes Konto per Passwort-Grant). Ohne PlatformAPIURL aus.
	PlatformAPIURL   string
	PlatformTokenURL string
	PlatformClientID string
	PlatformUser     string
	PlatformPassword string
	// Token-Austausch je Chat (RFC 8693) über einen vertraulichen Client, etwa agw-agent.
	PlatformClientSecret string
	PlatformExchange     bool
	PlatformAudiences    []string
	// Anmeldung an der API: AuthMode "token" (AGW_API_TOKEN, Standard) oder "oidc" (Anmeldung über
	// den Keycloak der Plattform, Authorization Code mit PKCE; Chats gehören dann dem Nutzer).
	AuthMode         string
	OIDCIssuer       string // https://keycloak.<basis>/realms/<realm>
	OIDCClientID     string // Standard agw-agent
	OIDCClientSecret string // Standard: AGW_PLATFORM_CLIENT_SECRET
	PublicURL        string // https://app.<basis>/agent oder https://agent.<basis>; Redirect-URI = PublicURL + "/oidc/callback"
	// BasePath: Pfad von PublicURL ("" oder etwa "/agent"); die UI liegt unter BasePath + "/". Der Proxy
	// schneidet ihn ab, Adressen für den Browser brauchen ihn trotzdem.
	BasePath string
	// FrameAncestors: Herkünfte, die die UI einbetten dürfen (leer: frame-ancestors 'none').
	FrameAncestors []string
}

// Anmeldearten der API (AGW_AUTH_MODE).
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
		EgressSubnet:         str("AGW_EGRESS_SUBNET", "10.231.20.0/24"), // nie Dockers 172.x (verdeckt sonst VPN-Ziele)
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
			"cli":   num("AGW_POOL_SIZE_CLI", 1),
			"mcp":   num("AGW_POOL_SIZE_MCP", 1),
			"beide": num("AGW_POOL_SIZE_BEIDE", 0),
			"api":   num("AGW_POOL_SIZE_API", 0),
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
		MaxSubagentsDefault:  num("AGW_MAX_SUBAGENTS_DEFAULT", 5),
		MaxSubagentsLimit:    num("AGW_MAX_SUBAGENTS_LIMIT", 20),
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

// orEmpty: ein ungültiger Wert fällt auf "" zurück; CheckAuth meldet den Fehler.
func orEmpty(s string, err error) string {
	if err != nil {
		return ""
	}
	return s
}

// BasePath liefert den Pfad von AGW_PUBLIC_URL ohne Schrägstrich am Ende: "" für https://host,
// "/agent" für https://host/agent/. Ein Proxy schneidet ihn vor dem Orchestrator ab; Adressen für den
// Browser (Weiterleitungen, Cookies, Anmeldelinks) brauchen ihn trotzdem.
func BasePath(publicURL string) (string, error) {
	if publicURL == "" {
		return "", nil
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", fmt.Errorf("AGW_PUBLIC_URL ungültig: %q", publicURL)
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
		return "", fmt.Errorf("AGW_PUBLIC_URL: Pfad %q ungültig (erlaubt etwa /agent)", u.Path)
	}
	return p, nil
}

// PublicHost liefert den Host von AGW_PUBLIC_URL (mit Port, ohne Pfad), "" ohne Angabe. Die UI wird
// unter dieser Adresse aufgerufen; der Host gehört deshalb zu den erlaubten Host-Kopfzeilen.
func (e Env) PublicHost() string {
	if u, err := url.Parse(e.PublicURL); err == nil {
		return u.Host
	}
	return ""
}

// CheckAuth prüft die Einstellungen der gewählten Anmeldeart.
func (e Env) CheckAuth() error {
	if _, err := BasePath(e.PublicURL); err != nil {
		return err
	}
	switch e.AuthMode {
	case AuthToken:
		if len(e.APIToken) < 32 {
			return errors.New("AGW_API_TOKEN fehlt oder ist kürzer als 32 Zeichen (./dev.sh init legt ihn an)")
		}
	case AuthOIDC:
		var missing []string
		for k, v := range map[string]string{"AGW_OIDC_ISSUER": e.OIDCIssuer, "AGW_OIDC_CLIENT_ID": e.OIDCClientID,
			"AGW_OIDC_CLIENT_SECRET (oder AGW_PLATFORM_CLIENT_SECRET)": e.OIDCClientSecret, "AGW_PUBLIC_URL": e.PublicURL} {
			if v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("AGW_AUTH_MODE=oidc verlangt %s", strings.Join(missing, ", "))
		}
		for _, o := range e.FrameAncestors {
			if !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://") && o != "'self'" {
				return fmt.Errorf("AGW_FRAME_ANCESTORS: %q ist keine Herkunft (https://…)", o)
			}
			if strings.ContainsAny(o, ";,") {
				return fmt.Errorf("AGW_FRAME_ANCESTORS: %q enthält ; oder ,", o)
			}
		}
	default:
		return fmt.Errorf("AGW_AUTH_MODE muss token oder oidc sein, nicht %q", e.AuthMode)
	}
	return nil
}

// keepAlive: AGW_BG_KEEPALIVE=0 heißt „Leerlauf nicht verschieben“ (-1 für den Manager).
func keepAlive(d time.Duration) time.Duration {
	if d <= 0 {
		return -1
	}
	return d
}

// wakes: AGW_BG_WAKES_PER_HOUR=0 (ebenso AGW_AUTO_TURNS_MAX=0) heißt „nie wecken“ (-1 für den
// Manager, dessen 0 die Vorgabe ist).
func wakes(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

// workspaceMax: AGW_WORKSPACE_MAX_MB=0 (oder negativ) schaltet die Sicherung des
// Arbeitsbereichs ab (-1).
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

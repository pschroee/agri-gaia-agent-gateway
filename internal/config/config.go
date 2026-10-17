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

	"agw/internal/toolset"
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
	// Input lists what the model accepts, as in pi's registry: "text", with "image" additionally for
	// models that read images (G1.12). Empty or without "image" means: text only. Nothing is guessed
	// from the model's name; the capabilities overview reports this field.
	Input []string `json:"input,omitempty"`
	// SelfHosted: the model runs on the user's own hardware (R0.9). Only then may it look at any
	// dataset's images; a cloud model only at the public ones (AGW_VISION_DATASET_PATTERN, P14).
	SelfHosted bool `json:"self_hosted,omitempty"`
	// Sampling lists which sampling fields the model takes (X3.6). A field that is false or absent is not
	// sent for an experiment run, because some providers reject unknown fields. The proxy only sets the
	// values; everything else of the request stays untouched.
	Sampling *Sampling `json:"sampling,omitempty"`
}

// Sampling: the sampling fields a model accepts (X3.6). The catalog states them per model; nothing is
// guessed from the model's name.
type Sampling struct {
	Temperature bool `json:"temperature"`
	Seed        bool `json:"seed"`
}

// TakesTemperature reports whether temperature may be sent to the model.
func (m Model) TakesTemperature() bool { return m.Sampling != nil && m.Sampling.Temperature }

// TakesSeed reports whether seed may be sent to the model.
func (m Model) TakesSeed() bool { return m.Sampling != nil && m.Sampling.Seed }

// ReadsImages reports whether the model accepts images. Without the field it does not: a model the
// catalog does not describe as reading images must not be promised to the user.
func (m Model) ReadsImages() bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
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
	// Toolsets: the bindings every new chat gets (AGW_TOOLSETS, comma-separated cli, mcp, api;
	// default cli). Raw value; Toolset() checks it (issue #29).
	Toolsets string
	// PoolSize: warm slots kept ready for the configured combination (AGW_POOL_SIZE, default 4).
	// Chats of another combination (stored before a change of AGW_TOOLSETS) get a slot on demand.
	PoolSize int
	// ExperimentPool: warm slots kept ready for experiment variants (AGW_EXPERIMENT_POOL, X0.3),
	// keyed by the pool key "exp:<binding>". Empty: experiment slots start on demand.
	ExperimentPool map[string]int
	// ExperimentUsers: accounts (preferred_username) that may create experiment chats
	// (AGW_EXPERIMENT_USERS, X0.2). Empty: no experiment chats.
	ExperimentUsers   []string
	IdleTimeout       time.Duration
	ApprovalTimeout   time.Duration
	ArtifactMaxBytes  int64
	ImageMaxBytes     int64 // display images in responses (AGW_IMAGE_MAX_MB)
	WorkspaceMaxBytes int64 // backup of /workspace per chat (AGW_WORKSPACE_MAX_MB; 0 = off → -1)
	SandboxMemoryMB   int64 // pi's container (pi and up to four subagents)
	SandboxCPUs       float64
	SandboxPids       int64
	ExecMemoryMB      int64 // execution sandbox
	ExecCPUs          float64
	ExecPids          int64
	DefaultModel      string
	TitleModel        string // model for chat titles ("provider/model"; empty: the chat's, "off": none)
	APIToken          string
	// MaxSubagents: at most this many subagents run at the same time per chat (AGW_MAX_SUBAGENTS,
	// default 5); fixed for the service, not a setting per chat.
	MaxSubagents int
	// ComputeMaxPerChat: compute calls per chat that the platform may accept (AGW_COMPUTE_MAX_PER_CHAT,
	// default 3; below 1 counts as 1). Enforced by the gateway, see internal/chat/effect.go.
	ComputeMaxPerChat int
	// A-E1: upper bounds of an autonomous plan's budget. The agent proposes, the gateway refuses
	// anything above these (plan.Limits). PlanMaxDuration never exceeds oidc.MaxSession - 1 h.
	PlanMaxDuration time.Duration
	PlanMaxLLMCost  float64

	// X3: limits of an experiment run at the model pass-through. The model, the provider and the upstream
	// address stay per run (the chat's model and the catalog), so a self hosted model can come later without
	// a change here. The values below are placeholders until the pilot runs fix them (XE5).
	ExpMaxCostUSD  float64       // AGW_EXP_MAX_COST_USD, default 0.50
	ExpMaxTokens   int64         // AGW_EXP_MAX_TOKENS, default 2 000 000 (input + output, without cache reads)
	ExpMaxDuration time.Duration // AGW_EXP_MAX_DURATION, default 90m, counted from the chat's creation
	ExpTemperature float64       // AGW_EXP_TEMPERATURE, default 0; sent where the catalog says the model takes it
	// ExpPlaceholderLimits: none of the limit variables was set, the defaults apply. The start logs a warning.
	ExpPlaceholderLimits bool

	// C-E3 (own code in the agent's notebook server). JupyterHubAPIURL empty: notebooks are off and
	// every notebook tool answers "not configured on this instance".
	JupyterHubAPIURL    string // AGW_JUPYTERHUB_API_URL: …/hub/api of the instance's JupyterHub
	JupyterHubToken     string // AGW_JUPYTERHUB_TOKEN: service token (scopes servers, read:users:name)
	JupyterHubPublicURL string // AGW_JUPYTERHUB_PUBLIC_URL: the address the user opens (links to a run)
	NotebookPrefix      string // AGW_NOTEBOOK_PREFIX: prefix of the agent servers in the hub (spawner hardening)
	// NotebookTimeoutMaxMin: upper limit of one run (AGW_NOTEBOOK_TIMEOUT_MAX_MIN, default 480 = the
	// longest an autonomous plan may run).
	NotebookTimeoutMaxMin int
	// NotebookDatasetMaxMB: largest dataset archive a run unpacks (AGW_NOTEBOOK_DATASET_MAX_MB).
	NotebookDatasetMaxMB int
	// NotebookIdleMin: a chat's notebook server is stopped after this long without a run
	// (AGW_NOTEBOOK_IDLE_MIN, 0 = never; the files stay).
	NotebookIdleMin int

	// R0 (result checks). The check itself needs no URL: without AGW_IMAGE or the container runtime
	// the capability "checks" is not available and every check tool answers so.
	// AssessMaxPerChat: how many checks one chat may use (AGW_ASSESS_MAX_PER_CHAT, default 12). A check
	// does not count against the compute calls (P7); failed checks do not count at all.
	AssessMaxPerChat int
	// AssessDownloadMaxMB: the largest dataset archive a check unpacks for its held-out images
	// (AGW_ASSESS_DOWNLOAD_MAX_MB, default 2048). R1 uses it when it downloads a dataset.
	AssessDownloadMaxMB int
	// VisionDatasetPattern: the names of the datasets whose images may go to a cloud model
	// (AGW_VISION_DATASET_PATTERN, default ^(mnist|test)[-_]). A self hosted model may see all (P14).
	VisionDatasetPattern string

	// R3 (deployment check). EdgeSimDevices are the simulated edge devices whose deployments the
	// gateway may probe with test requests (AGW_EDGE_SIM_DEVICES, "name=container" pairs). The list is
	// the operator's: a device that is not in it is out of reach, whatever the platform says about it,
	// and a real device can never be reached this way (RF1 of the planning). Empty: no test requests.
	EdgeSimDevices []EdgeSimDevice

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
	// TrashRetentionDays: chats in the trash are deleted permanently after this many whole days
	// (AGW_TRASH_RETENTION_DAYS; default 0 = never, negative counts as 0).
	TrashRetentionDays int
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
	// PlatformTokenMaxAge: an exchanged token is used this long at most (AGW_PLATFORM_TOKEN_MAX_AGE, default 5m).
	PlatformTokenMaxAge time.Duration
	// Experiment accounts (X4, issue #129): the two accounts whose platform objects the fixtures are.
	// Primary is the run's own account (agent-exp), Secondary a second one whose objects do not belong
	// to the user (agent-exp-2, XS2). The passwords stay in the secret file on the instance.
	ExpUser      string
	ExpPassword  string
	Exp2User     string
	Exp2Password string
	// ExpAPIToken protects /api/exp/* (AGW_EXP_API_TOKEN). Empty: the fixture routes answer 503.
	// X1.3: it also reaches POST /api/chats with an experiment profile and GET routes of experiment
	// chats, and it needs at least 32 characters.
	ExpAPIToken string
	// ExpClientID: the client of the experiment account's password grant (AGW_EXP_CLIENT_ID,
	// default frontend, X1.2).
	ExpClientID string

	// V1 — pre-annotation with CVAT. The service is wired only when both URLs are set; without them
	// the path /_agw/annotation answers "not configured" and the capability annotation says so.
	CVATAPIURL string // AGW_CVAT_API_URL: the API of the CVAT server (https://cvat.<base>)
	// CVATPublicURL: the address of CVAT for the user's browser (default: the API URL); the answers
	// carry links to the tasks.
	CVATPublicURL string
	// AnnotationFunctions: function ids the operator installed (AGW_ANNOTATION_FUNCTIONS, comma
	// separated, default none). Only these and the user's own functions can be run (E7).
	AnnotationFunctions []string
	// AnnotationMaxFunctions (AGW_ANNOTATION_MAX_FUNCTIONS, default 3) and AnnotationModelMaxMB
	// (AGW_ANNOTATION_MODEL_MAX_MB, default 300) bound the functions a user may have built from their
	// own models; V2 enforces them.
	AnnotationMaxFunctions int
	AnnotationModelMaxMB   int64

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

// DefaultPoolSize: warm slots for the configured combination unless AGW_POOL_SIZE says otherwise.
// Four (decision of the author, 2026-10-07): an idle warm slot needs about 100–210 MB in pi's
// container plus about 3 MB in the execution sandbox (measured on the instance), so several chats
// started in a row do not wait for a cold start.
const DefaultPoolSize = 4

// LegacyPoolSizeVars are the pool sizes per variant before issue #29; they are ignored now, and the
// orchestrator warns at startup when one is set.
var LegacyPoolSizeVars = []string{"AGW_POOL_SIZE_CLI", "AGW_POOL_SIZE_MCP", "AGW_POOL_SIZE_BOTH", "AGW_POOL_SIZE_API"}

// toolsetsEnv reads AGW_TOOLSETS. Unlike other variables an empty value is not replaced by the
// default: a variable set to "" is a configuration error that Toolset() reports.
func toolsetsEnv() string {
	if v, ok := os.LookupEnv(toolset.Env); ok {
		return v
	}
	return toolset.Default
}

// Toolset checks AGW_TOOLSETS; an error stops the start.
func (e Env) Toolset() (toolset.Set, error) {
	return toolset.Parse(e.Toolsets)
}

// DefaultMaxSubagents: subagents that may run at the same time per chat unless AGW_MAX_SUBAGENTS says
// otherwise (issue #24: fixed, no setting per chat).
const DefaultMaxSubagents = 5

// C-E3 (own code in the agent's notebook server): the defaults of AGW_NOTEBOOK_*.
const (
	// DefaultNotebookPrefix: the spawner of the instance only hardens servers whose name starts with
	// this prefix (plan E2, platform_spawner.py AGENT_PREFIX "agw-"), so the gateway must never start
	// a server without it: an unhardened server would run the agent's code with the user's privileges.
	DefaultNotebookPrefix = "agw"
	// DefaultNotebookTimeoutMaxMin: 480 minutes is the longest an autonomous plan may run
	// (AGW_PLAN_MAX_DURATION, 8 h), so a run inside a plan cannot outlive the plan.
	DefaultNotebookTimeoutMaxMin = 480
	DefaultNotebookDatasetMaxMB  = 2048
	DefaultNotebookIdleMin       = 60
)

// R0 (result checks): the defaults of AGW_ASSESS_* and the image review.
const (
	// DefaultAssessMaxPerChat: 12 checks per chat. The whole way through the work needs six compute
	// calls (P7); a check repeats a few times while a model is being improved, and the limit stops a
	// loop that checks without end.
	DefaultAssessMaxPerChat = 12
	// DefaultAssessDownloadMaxMB: the largest dataset archive a check unpacks (2048 MB, as for a
	// notebook run).
	DefaultAssessDownloadMaxMB = 2048
	// DefaultVisionDatasetPattern: public example sets, whose images may go to a cloud model (P14).
	DefaultVisionDatasetPattern = `^(mnist|test)[-_]`
)

// Login modes of the API (AGW_AUTH_MODE).
const (
	AuthToken = "token"
	AuthOIDC  = "oidc"
)

// A-E1: upper bounds of an autonomous plan's budget, clamped so that a plan cannot outlive the login
// session (oidc.MaxSession is 12 h). DefaultPlanDuration/DefaultPlanLLMCost are the caps unless the
// environment says otherwise; plan.DefaultPlanDurationMinutes and the other defaults live in internal/plan.
const (
	DefaultPlanMaxDuration = 8 * time.Hour
	DefaultPlanMaxLLMCost  = 5.00
	// PlanDurationCap never exceeds oidc.MaxSession (12 h) minus one hour of headroom.
	PlanDurationCap = 11 * time.Hour
)

// planMaxDuration reads AGW_PLAN_MAX_DURATION and clamps it to PlanDurationCap; a value below one minute
// counts as one minute.
func planMaxDuration() time.Duration {
	d := dur("AGW_PLAN_MAX_DURATION", DefaultPlanMaxDuration)
	if d > PlanDurationCap {
		d = PlanDurationCap
	}
	if d < time.Minute {
		d = time.Minute
	}
	return d
}

// planMaxLLMCost reads AGW_PLAN_MAX_LLM_COST; a value below 0.01 counts as 0.01.
func planMaxLLMCost() float64 {
	c := flt("AGW_PLAN_MAX_LLM_COST", DefaultPlanMaxLLMCost)
	if c < 0.01 {
		c = 0.01
	}
	return c
}

func FromEnv() Env {
	exp := expLimits()
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
		Toolsets:             toolsetsEnv(),
		PoolSize:             max(0, num("AGW_POOL_SIZE", DefaultPoolSize)),
		ExperimentPool:       experimentPool(),
		ExperimentUsers:      commaList("AGW_EXPERIMENT_USERS"),
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
		ComputeMaxPerChat:    max(1, num("AGW_COMPUTE_MAX_PER_CHAT", DefaultComputeMaxPerChat)),
		PlanMaxDuration:      planMaxDuration(),
		PlanMaxLLMCost:       planMaxLLMCost(),

		// X3: limits of an experiment run (placeholders until XE5).
		ExpMaxCostUSD:        exp.MaxCostUSD,
		ExpMaxTokens:         exp.MaxTokens,
		ExpMaxDuration:       exp.MaxDuration,
		ExpTemperature:       exp.Temperature,
		ExpPlaceholderLimits: exp.PlaceholderSet,

		// C-E3 (own code in the agent's notebook server).
		JupyterHubAPIURL:      strings.TrimRight(str("AGW_JUPYTERHUB_API_URL", ""), "/"),
		JupyterHubToken:       str("AGW_JUPYTERHUB_TOKEN", ""),
		JupyterHubPublicURL:   strings.TrimRight(str("AGW_JUPYTERHUB_PUBLIC_URL", ""), "/"),
		NotebookPrefix:        str("AGW_NOTEBOOK_PREFIX", DefaultNotebookPrefix),
		NotebookTimeoutMaxMin: max(1, num("AGW_NOTEBOOK_TIMEOUT_MAX_MIN", DefaultNotebookTimeoutMaxMin)),
		NotebookDatasetMaxMB:  max(1, num("AGW_NOTEBOOK_DATASET_MAX_MB", DefaultNotebookDatasetMaxMB)),
		NotebookIdleMin:       max(0, num("AGW_NOTEBOOK_IDLE_MIN", DefaultNotebookIdleMin)),

		// R0 (result checks).
		AssessMaxPerChat:     max(1, num("AGW_ASSESS_MAX_PER_CHAT", DefaultAssessMaxPerChat)),
		AssessDownloadMaxMB:  max(1, num("AGW_ASSESS_DOWNLOAD_MAX_MB", DefaultAssessDownloadMaxMB)),
		VisionDatasetPattern: str("AGW_VISION_DATASET_PATTERN", DefaultVisionDatasetPattern),

		// R3 (deployment check): the simulated edge devices the gateway may send test requests to.
		EdgeSimDevices: edgeSimDevices("AGW_EDGE_SIM_DEVICES"),

		AllowedHosts:         strings.Split(str("AGW_ALLOWED_HOSTS", "127.0.0.1:18480,localhost:18480"), ","),
		BgMax:                max(1, num("AGW_BG_MAX", 5)),
		BgWakesPerHour:       wakes(num("AGW_BG_WAKES_PER_HOUR", 10)),
		BgKeepAlive:          keepAlive(dur("AGW_BG_KEEPALIVE", time.Hour)),
		AutoTurnsMax:         wakes(num("AGW_AUTO_TURNS_MAX", 5)),
		TrashRetentionDays:   max(0, num("AGW_TRASH_RETENTION_DAYS", 0)),
		PlatformAPIURL:       str("AGW_PLATFORM_API_URL", ""),
		PlatformTokenURL:     str("AGW_PLATFORM_TOKEN_URL", ""),
		PlatformClientID:     str("AGW_PLATFORM_CLIENT_ID", "frontend"),
		PlatformUser:         str("AGW_PLATFORM_USER", ""),
		PlatformPassword:     str("AGW_PLATFORM_PASSWORD", ""),
		PlatformClientSecret: str("AGW_PLATFORM_CLIENT_SECRET", ""),
		PlatformExchange:     str("AGW_PLATFORM_TOKEN_EXCHANGE", "false") == "true",
		PlatformAudiences:    strings.Split(str("AGW_PLATFORM_AUDIENCES", "backend,minio"), ","),
		PlatformTokenMaxAge:  dur("AGW_PLATFORM_TOKEN_MAX_AGE", 5*time.Minute),
		// X4: the two experiment accounts and the key of the fixture routes.
		ExpUser:      str("AGW_EXP_USER", ""),
		ExpPassword:  str("AGW_EXP_PASSWORD", ""),
		Exp2User:     str("AGW_EXP2_USER", ""),
		Exp2Password: str("AGW_EXP2_PASSWORD", ""),
		ExpAPIToken:  str("AGW_EXP_API_TOKEN", ""),
		ExpClientID:  str("AGW_EXP_CLIENT_ID", "frontend"),

		// V1 — pre-annotation with CVAT.
		CVATAPIURL:          strings.TrimRight(str("AGW_CVAT_API_URL", ""), "/"),
		CVATPublicURL:       strings.TrimRight(str("AGW_CVAT_PUBLIC_URL", str("AGW_CVAT_API_URL", "")), "/"),
		AnnotationFunctions: commaList("AGW_ANNOTATION_FUNCTIONS"),
		// V2 bounds the functions of one user; the values are read here so that the instance has them.
		AnnotationMaxFunctions: max(0, num("AGW_ANNOTATION_MAX_FUNCTIONS", 3)),
		AnnotationModelMaxMB:   int64(max(0, num("AGW_ANNOTATION_MODEL_MAX_MB", 300))),

		AuthMode:         strings.ToLower(strings.TrimSpace(str("AGW_AUTH_MODE", AuthToken))),
		OIDCIssuer:       strings.TrimRight(str("AGW_OIDC_ISSUER", ""), "/"),
		OIDCClientID:     str("AGW_OIDC_CLIENT_ID", "agw-agent"),
		OIDCClientSecret: str("AGW_OIDC_CLIENT_SECRET", str("AGW_PLATFORM_CLIENT_SECRET", "")),
		PublicURL:        strings.TrimRight(str("AGW_PUBLIC_URL", ""), "/"),
		BasePath:         orEmpty(BasePath(strings.TrimRight(str("AGW_PUBLIC_URL", ""), "/"))),
		FrameAncestors:   strings.Fields(str("AGW_FRAME_ANCESTORS", "")),
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

// EdgeSimDevice is one simulated edge device the gateway may send test requests to (R3): the name the
// platform knows the device by and the container that runs it on the platform host.
type EdgeSimDevice struct {
	Name      string
	Container string
}

// commaList reads a comma-separated list (V1: the function ids of the operator); spaces are trimmed
// and empty entries dropped, so a value that is only spaces gives an empty list.
func commaList(k string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(k), ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// edgeSimDevices reads AGW_EDGE_SIM_DEVICES: "name=container" pairs, comma separated (R3). An entry
// without "=" or with an empty side is dropped — a name the operator mistyped must not become a device
// the gateway may exec into.
func edgeSimDevices(k string) []EdgeSimDevice {
	var out []EdgeSimDevice
	for _, part := range commaList(k) {
		name, container, ok := strings.Cut(part, "=")
		name, container = strings.TrimSpace(name), strings.TrimSpace(container)
		if !ok || name == "" || container == "" {
			continue
		}
		out = append(out, EdgeSimDevice{Name: name, Container: container})
	}
	return out
}

// experimentPool reads AGW_EXPERIMENT_POOL: "variant=n" pairs, comma separated, for example
// "exp:mcp=1,exp:api=1" (X0.3). The prefix "exp:" may be left out; the binding is checked and the
// key normalized. An entry with an unknown binding or a count below 1 is dropped, so that a typo does
// not keep a slot of a variant nobody can use. A combination key contains a comma itself, so it
// cannot be written here; such experiment variants start on demand.
func experimentPool() map[string]int {
	out := map[string]int{}
	for _, part := range commaList("AGW_EXPERIMENT_POOL") {
		variant, count, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		base, _ := toolset.SplitExperiment(strings.TrimSpace(variant))
		key, err := toolset.Normalize(base)
		n, nerr := strconv.Atoi(strings.TrimSpace(count))
		if err != nil || nerr != nil || n < 1 {
			continue
		}
		out[toolset.ExperimentPrefix+key] = n
	}
	return out
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

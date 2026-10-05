package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const catalogJSON = `{
  "default": "deepseek/deepseek-flash",
  "providers": [{
    "id": "deepseek", "upstream": "https://api.deepseek.com", "api": "openai-completions",
    "api_key_env": "DEEPSEEK_API_KEY",
    "models": [
      {"id": "deepseek-flash", "name": "DeepSeek V4.1 Flash", "reasoning": true, "context_window": 1048576, "max_tokens": 65536,
       "pricing": {"input": 0.3, "output": 1.2, "cache_read": 0.006, "cache_write": 0, "note": "peak"}},
      {"id": "deepseek-v4-pro", "name": "Pro"}
    ]
  }]
}`

func TestParseCatalogAndLookup(t *testing.T) {
	c, err := ParseCatalog([]byte(catalogJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.Default != "deepseek/deepseek-flash" {
		t.Fatalf("default: %q", c.Default)
	}
	p, m, ok := c.Lookup("deepseek/deepseek-flash")
	if !ok || p.ID != "deepseek" || m.ID != "deepseek-flash" {
		t.Fatalf("Lookup failed: %v %v %v", p, m, ok)
	}
	if _, _, ok := c.Lookup("deepseek/doesnotexist"); ok {
		t.Fatal("unknown model found")
	}
	if _, _, ok := c.Lookup("no-slash"); ok {
		t.Fatal("identifier without provider accepted")
	}
	ms := c.Models()
	if len(ms) != 2 || !ms[0].Default || ms[1].Default || ms[0].Pricing == nil || ms[0].Pricing.Currency != "USD" {
		t.Fatalf("Models(): %+v", ms)
	}
}

func TestParseCatalogRejectsUnknownDefault(t *testing.T) {
	bad := strings.Replace(catalogJSON, `"default": "deepseek/deepseek-flash"`, `"default": "x/y"`, 1)
	if _, err := ParseCatalog([]byte(bad)); err == nil {
		t.Fatal("error expected")
	}
}

// The models.json for pi must not contain a real key and must point to
// the orchestrator's proxy.
func TestPiModelsJSONPointsToProxyWithoutKey(t *testing.T) {
	c, _ := ParseCatalog([]byte(catalogJSON))
	t.Setenv("DEEPSEEK_API_KEY", "sk-secret-123")
	b, err := c.PiModelsJSON("http://orchestrator:18481")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-secret") {
		t.Fatal("key in pi configuration")
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID        string `json:"id"`
				Reasoning bool   `json:"reasoning"`
				Cost      struct {
					Input, Output, CacheRead float64
				} `json:"cost"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Providers["deepseek"]
	if p.BaseURL != "http://orchestrator:18481/llm/deepseek" || p.API != "openai-completions" || p.APIKey == "" {
		t.Fatalf("wrong provider: %+v", p)
	}
	if len(p.Models) != 2 || p.Models[0].ID != "deepseek-flash" || !p.Models[0].Reasoning || p.Models[0].Cost.Output != 1.2 || p.Models[0].Cost.CacheRead != 0.006 {
		t.Fatalf("wrong models: %+v", p.Models)
	}
}

func TestUpstreamKey(t *testing.T) {
	c, _ := ParseCatalog([]byte(catalogJSON))
	t.Setenv("DEEPSEEK_API_KEY", "sk-x")
	p, _, _ := c.Lookup("deepseek/deepseek-flash")
	if p.APIKey() != "sk-x" {
		t.Fatal("key not read from the environment")
	}
}

func TestEnvDefaults(t *testing.T) {
	t.Setenv("AGW_POOL_SIZE_CLI", "")
	t.Setenv("AGW_POOL_SIZE_MCP", "3")
	t.Setenv("AGW_IDLE_TIMEOUT", "90s")
	e := FromEnv()
	if e.HTTPAddr != ":18480" || e.ProxyAddr != ":18481" {
		t.Fatalf("addresses: %q %q", e.HTTPAddr, e.ProxyAddr)
	}
	if e.PoolSizes["cli"] != 1 || e.PoolSizes["mcp"] != 3 || e.PoolSizes["both"] != 0 {
		t.Fatalf("pool sizes: %v", e.PoolSizes)
	}
	if e.IdleTimeout.Seconds() != 90 || e.ApprovalTimeout.Minutes() != 10 || e.ArtifactMaxBytes != 50<<20 || e.ImageMaxBytes != 10<<20 {
		t.Fatalf("timeouts/limits: %+v", e)
	}
	if e.WorkspaceMaxBytes != 200<<20 {
		t.Fatalf("workspace limit: %d", e.WorkspaceMaxBytes)
	}
	t.Setenv("AGW_WORKSPACE_MAX_MB", "0")
	if e := FromEnv(); e.WorkspaceMaxBytes != -1 {
		t.Fatalf("AGW_WORKSPACE_MAX_MB=0 should turn off the backup: %d", e.WorkspaceMaxBytes)
	}
}

const builtinCatalog = `{
  "default": "deepseek/deepseek-flash",
  "providers": [{
    "id": "deepseek", "upstream": "https://api.deepseek.com", "api": "openai-completions", "api_key_env": "DEEPSEEK_API_KEY",
    "tariff": {"peak_windows_utc": [], "offpeak_factor": 1, "source": "https://example.org/pricing", "retrieved": "2026-09-29"},
    "models": [
      {"id": "deepseek-flash", "pi_builtin": true, "note": "peak tariff"},
      {"id": "own", "name": "Own model", "pricing": {"input": 1, "output": 2}}
    ]
  }]
}`

// Models that pi knows itself are not redefined: pi keeps its compat settings
// and prices; only baseUrl and key are redirected.
func TestBuiltinModelsAreNotRedefined(t *testing.T) {
	c, err := ParseCatalog([]byte(builtinCatalog))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.PiModelsJSON("http://orchestrator:18481")
	var doc struct {
		Providers map[string]struct {
			BaseURL string                `json:"baseUrl"`
			APIKey  string                `json:"apiKey"`
			Models  []struct{ ID string } `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Providers["deepseek"]
	if p.BaseURL != "http://orchestrator:18481/llm/deepseek" || p.APIKey == "" {
		t.Fatalf("redirect missing: %+v", p)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "own" {
		t.Fatalf("only the own model may be defined: %+v", p.Models)
	}
}

func TestPricingFromPiRegistry(t *testing.T) {
	c, _ := ParseCatalog([]byte(builtinCatalog))
	ms := c.Models()
	if ms[0].Pricing != nil {
		t.Fatalf("no prices without registry: %+v", ms[0].Pricing)
	}
	c.SetRegistry("0.87.1", []RegistryModel{
		{Provider: "deepseek", ID: "deepseek-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 1000000,
			Cost: RegistryCost{Input: 0.3, Output: 1.2, CacheRead: 0.006}},
		{Provider: "deepseek", ID: "own", Cost: RegistryCost{Input: 9}},
		{Provider: "other", ID: "x"},
	})
	ms = c.Models()
	flash, own := ms[0], ms[1]
	if flash.Name != "DeepSeek V4.1 Flash" || flash.Pricing == nil || flash.Pricing.Output != 1.2 || flash.Pricing.CacheRead != 0.006 {
		t.Fatalf("registry not taken over: %+v %+v", flash, flash.Pricing)
	}
	if flash.Pricing.Source != "https://example.org/pricing" || flash.Pricing.Retrieved != "2026-09-29" {
		t.Fatalf("source missing: %+v", flash.Pricing)
	}
	if !strings.Contains(flash.Pricing.Note, "peak tariff") || !strings.Contains(flash.Pricing.Note, "pi 0.87.1") {
		t.Fatalf("note: %q", flash.Pricing.Note)
	}
	if own.Pricing.Input != 1 {
		t.Fatalf("own prices must take precedence: %+v", own.Pricing)
	}
	if len(ms) != 2 {
		t.Fatalf("foreign registry models must not appear: %d", len(ms))
	}
}

const tariffCatalog = `{
  "default": "deepseek/deepseek-flash",
  "providers": [{
    "id": "deepseek", "api": "openai-completions",
    "tariff": {"peak_windows_utc": [{"days": "mon-fri", "from": "01:00", "to": "04:00"}, {"days": "mon-fri", "from": "06:00", "to": "10:00"}], "offpeak_factor": 0.5},
    "models": [{"id": "deepseek-flash", "pricing": {"input": 0.3, "output": 1.2, "cache_read": 0.006}}]
  }]
}`

func TestCostWithPeakTariff(t *testing.T) {
	c, err := ParseCatalog([]byte(tariffCatalog))
	if err != nil {
		t.Fatal(err)
	}
	u := Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	full := 0.3 + 1.2 + 0.006
	cases := []struct {
		at   string
		peak bool
	}{
		{"2026-09-29T02:30:00Z", true},  // Tuesday, peak
		{"2026-09-29T04:00:00Z", false}, // end exclusive
		{"2026-09-29T05:59:59Z", false},
		{"2026-09-29T06:00:00Z", true},
		{"2026-09-29T12:00:00Z", false},
		{"2026-10-03T02:00:00Z", false}, // Saturday
		{"2026-10-05T09:59:00Z", true},  // Monday
	}
	for _, tc := range cases {
		at, _ := time.Parse(time.RFC3339, tc.at)
		cost, peak, ok := c.Cost("deepseek/deepseek-flash", u, at)
		want := full
		if !tc.peak {
			want = full / 2
		}
		if !ok || peak != tc.peak || cost < want-1e-9 || cost > want+1e-9 {
			t.Errorf("%s: cost %v peak %v, expected %v %v", tc.at, cost, peak, want, tc.peak)
		}
	}
	if _, _, ok := c.Cost("x/y", u, time.Now()); ok {
		t.Fatal("unknown model billed")
	}
}

func TestCostWithoutTariffIsFlat(t *testing.T) {
	c, _ := ParseCatalog([]byte(catalogJSON))
	cost, peak, ok := c.Cost("deepseek/deepseek-flash", Usage{Output: 1_000_000}, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	if !ok || cost != 1.2 || peak {
		t.Fatalf("without tariff: %v %v %v", cost, peak, ok)
	}
}

func TestBadTariffRejected(t *testing.T) {
	bad := strings.Replace(tariffCatalog, `"from": "01:00"`, `"from": "1 o'clock"`, 1)
	if _, err := ParseCatalog([]byte(bad)); err == nil {
		t.Fatal("invalid time accepted")
	}
	overnight := strings.Replace(tariffCatalog, `"from": "01:00", "to": "04:00"`, `"from": "22:00", "to": "02:00"`, 1)
	if _, err := ParseCatalog([]byte(overnight)); err == nil {
		t.Fatal("window across midnight accepted")
	}
}

func TestBackgroundEnv(t *testing.T) {
	e := FromEnv()
	if e.BgMax != 5 || e.BgWakesPerHour != 10 || e.BgKeepAlive != time.Hour || e.AutoTurnsMax != 5 {
		t.Fatalf("defaults: %d %d %v", e.BgMax, e.BgWakesPerHour, e.BgKeepAlive)
	}
	t.Setenv("AGW_BG_MAX", "0")
	t.Setenv("AGW_BG_WAKES_PER_HOUR", "0")
	t.Setenv("AGW_BG_KEEPALIVE", "0")
	t.Setenv("AGW_AUTO_TURNS_MAX", "0")
	e = FromEnv()
	if e.BgMax != 1 || e.BgWakesPerHour != -1 || e.BgKeepAlive != -1 {
		t.Fatalf("turned off: %d %d %v", e.BgMax, e.BgWakesPerHour, e.BgKeepAlive)
	}
	if e.AutoTurnsMax != -1 {
		t.Fatalf("AGW_AUTO_TURNS_MAX=0: %d", e.AutoTurnsMax)
	}
	t.Setenv("AGW_BG_MAX", "3")
	t.Setenv("AGW_BG_WAKES_PER_HOUR", "4")
	t.Setenv("AGW_BG_KEEPALIVE", "30m")
	t.Setenv("AGW_AUTO_TURNS_MAX", "2")
	e = FromEnv()
	if e.BgMax != 3 || e.BgWakesPerHour != 4 || e.BgKeepAlive != 30*time.Minute || e.AutoTurnsMax != 2 {
		t.Fatalf("set: %d %d %v", e.BgMax, e.BgWakesPerHour, e.BgKeepAlive)
	}
}

func TestCheckAuth(t *testing.T) {
	tok := strings.Repeat("a", 32)
	oidc := Env{AuthMode: AuthOIDC, OIDCIssuer: "https://kc.example/realms/r", OIDCClientID: "agw-agent", OIDCClientSecret: "s", PublicURL: "https://agent.example"}
	cases := []struct {
		name string
		env  Env
		ok   bool
	}{
		{"token", Env{AuthMode: AuthToken, APIToken: tok}, true},
		{"token too short", Env{AuthMode: AuthToken, APIToken: "short"}, false},
		{"oidc without token", oidc, true},
		{"oidc without issuer", func() Env { e := oidc; e.OIDCIssuer = ""; return e }(), false},
		{"oidc without secret", func() Env { e := oidc; e.OIDCClientSecret = ""; return e }(), false},
		{"oidc with embedding", func() Env { e := oidc; e.FrameAncestors = []string{"https://app.example"}; return e }(), true},
		{"oidc with broken origin", func() Env { e := oidc; e.FrameAncestors = []string{"app.example;"}; return e }(), false},
		{"unknown", Env{AuthMode: "basic"}, false},
	}
	for _, c := range cases {
		if err := c.env.CheckAuth(); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestOIDCEnvDefaults(t *testing.T) {
	t.Setenv("AGW_AUTH_MODE", "OIDC")
	t.Setenv("AGW_PLATFORM_CLIENT_SECRET", "platform")
	t.Setenv("AGW_FRAME_ANCESTORS", "https://app.a  https://app.b")
	t.Setenv("AGW_PUBLIC_URL", "https://agent.example/")
	e := FromEnv()
	if e.AuthMode != AuthOIDC || e.OIDCClientID != "agw-agent" || e.OIDCClientSecret != "platform" || e.PublicURL != "https://agent.example" {
		t.Fatalf("%+v", e)
	}
	if len(e.FrameAncestors) != 2 || e.FrameAncestors[1] != "https://app.b" {
		t.Fatalf("frame ancestors: %v", e.FrameAncestors)
	}
}

func TestBasePath(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		"https://agent.x":           "",
		"https://agent.x/":          "",
		"https://app.x/agent":       "/agent",
		"https://app.x/agent/":      "/agent",
		"http://localhost:8080/a/b": "/a/b",
	} {
		if got, err := BasePath(in); err != nil || got != want {
			t.Errorf("BasePath(%q) = %q, %v; expected %q", in, got, err, want)
		}
	}
	for _, in := range []string{"https://app.x/a/../b", "https://app.x//agent", "https://app.x/a%20b", "https://app.x/a%2Fb", "https://app.x/./a"} {
		if got, err := BasePath(in); err == nil {
			t.Errorf("BasePath(%q) accepted: %q", in, got)
		}
	}
}

func TestEnvPublicURLWithPath(t *testing.T) {
	t.Setenv("AGW_PUBLIC_URL", "https://app.agri-gaia.localhost/agent/")
	e := FromEnv()
	if e.PublicURL != "https://app.agri-gaia.localhost/agent" || e.BasePath != "/agent" {
		t.Fatalf("PublicURL/BasePath: %q %q", e.PublicURL, e.BasePath)
	}
	// The host counts as allowed even if the address carries a path.
	if h := e.PublicHost(); h != "app.agri-gaia.localhost" {
		t.Fatalf("PublicHost: %q", h)
	}
	e.AuthMode, e.OIDCIssuer, e.OIDCClientID, e.OIDCClientSecret = AuthOIDC, "https://kc/realms/r", "agw-agent", "s"
	e.FrameAncestors = []string{"'self'", "https://app.agri-gaia.localhost"}
	if err := e.CheckAuth(); err != nil {
		t.Fatalf("CheckAuth: %v", err)
	}
	e.PublicURL = "https://app.agri-gaia.localhost/a/../b"
	if err := e.CheckAuth(); err == nil {
		t.Fatal("invalid path accepted")
	}
	t.Setenv("AGW_PUBLIC_URL", "")
	if e := FromEnv(); e.BasePath != "" || e.PublicHost() != "" {
		t.Fatalf("without AGW_PUBLIC_URL: %q %q", e.BasePath, e.PublicHost())
	}
}

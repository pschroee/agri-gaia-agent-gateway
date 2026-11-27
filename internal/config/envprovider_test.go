// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "k-super-secret-value-1"

// envMap makes a getenv over a map; the base setting is a minimal self-hosted provider with a key.
func envMap(over map[string]string) func(string) string {
	m := map[string]string{
		"AGW_LLM_PROVIDER":       "farm",
		"AGW_LLM_BASE_URL":       "https://llm.example.internal/v1",
		"AGW_LLM_MODEL_ID":       "Org/Model-27B",
		"AGW_LLM_API_KEY_ENV":    "FARM_API_KEY",
		"AGW_LLM_CONTEXT_WINDOW": "262144",
		"AGW_LLM_MAX_TOKENS":     "12288",
		"FARM_API_KEY":           testKey,
	}
	for k, v := range over {
		if v == "-" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func TestEnvProviderMinimal(t *testing.T) {
	p, model, extra, err := EnvProvider(envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || model != "" || extra != "" {
		t.Fatalf("provider %v, AGW_MODEL %q, extra %q", p, model, extra)
	}
	if p.ID != "farm" || p.Upstream != "https://llm.example.internal/v1" || p.API != "openai-completions" || p.APIKeyEnv != "FARM_API_KEY" {
		t.Fatalf("provider: %+v", p)
	}
	m := p.Models[0]
	if m.ID != "Org/Model-27B" || m.ContextWindow != 262144 || m.MaxTokens != 12288 || m.SelfHosted || m.ReadsImages() {
		t.Fatalf("model: %+v", m)
	}
	if m.Thinking != "user" || m.ThinkingLevelMap != nil || !m.Reasoning {
		t.Fatalf("thinking: %+v", m)
	}
	// No provider: nothing is built.
	if p, _, _, err := EnvProvider(func(string) string { return "" }); p != nil || err != nil {
		t.Fatalf("empty: %v %v", p, err)
	}
	// A setting without a provider would be ignored silently: refused.
	if _, _, _, err := EnvProvider(func(k string) string {
		if k == "AGW_LLM_TAGS" {
			return "gb10"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "AGW_LLM_TAGS") {
		t.Fatalf("stray variable: %v", err)
	}
}

func TestEnvProviderFullQwenSetting(t *testing.T) {
	p, _, extra, err := EnvProvider(envMap(map[string]string{
		"AGW_LLM_INPUT":            "text,image",
		"AGW_LLM_THINKING":         "on",
		"AGW_LLM_TAGS":             "gb10",
		"AGW_LLM_SAMPLING":         "seed",
		"AGW_LLM_INSTANCE_HEADERS": "x-litellm-model-api-base, x-litellm-model-id",
		"AGW_LLM_EXTRA_HOST":       "llm.example.internal:192.0.2.10",
		"AGW_LLM_PROVIDER_JSON":    `{"compat":{"thinkingFormat":"qwen-chat-template","maxTokensField":"max_tokens","supportsDeveloperRole":false,"supportsReasoningEffort":false,"supportsStore":false}}`,
		"AGW_LLM_MODEL_JSON":       `{"samplingParams":{"top_p":0.95}}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := p.Models[0]
	if !m.ReadsImages() || !m.TakesSeed() || m.TakesTemperature() || extra != "llm.example.internal:192.0.2.10" {
		t.Fatalf("model: %+v, extra %q", m, extra)
	}
	if len(m.RequestTags) != 1 || m.RequestTags[0] != "gb10" || len(m.InstanceHeaders) != 2 || m.InstanceHeaders[1] != "x-litellm-model-id" {
		t.Fatalf("tags/headers: %+v", m)
	}
	if p.ThinkingFormat(m) != "qwen-chat-template" {
		t.Fatalf("thinking format: %q", p.ThinkingFormat(m))
	}
	// The title call gets the tag and no thinking.
	var title map[string]any
	if err := json.Unmarshal(p.TitleRequest, &title); err != nil {
		t.Fatal(err)
	}
	kw, _ := title["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false || title["tags"] == nil {
		t.Fatalf("title request: %v", title)
	}
}

func TestEnvProviderUnknownField(t *testing.T) {
	for _, c := range []struct{ variable, json, field string }{
		{"AGW_LLM_PROVIDER_JSON", `{"compat":{"thinkingFormt":"qwen"}}`, "thinkingFormt"},
		{"AGW_LLM_PROVIDER_JSON", `{"nonsense":1}`, "nonsense"},
		{"AGW_LLM_MODEL_JSON", `{"samplngParams":{}}`, "samplngParams"},
		{"AGW_LLM_MODEL_JSON", `{"compat":{"maxTokenField":"max_tokens"}}`, "maxTokenField"},
	} {
		_, _, _, err := EnvProvider(envMap(map[string]string{c.variable: c.json}))
		if err == nil || !strings.Contains(err.Error(), c.variable) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s %s: %v", c.variable, c.json, err)
		}
	}
}

func TestEnvProviderWrongType(t *testing.T) {
	for _, c := range []struct{ variable, json, field string }{
		{"AGW_LLM_PROVIDER_JSON", `{"compat":{"supportsStore":"no"}}`, "supportsStore"},
		{"AGW_LLM_MODEL_JSON", `{"compat":{"maxTokensField":5}}`, "maxTokensField"},
		{"AGW_LLM_MODEL_JSON", `{"headers":{"X-A":1}}`, "headers"},
		{"AGW_LLM_PROVIDER_JSON", `[1]`, ""},
		{"AGW_LLM_MODEL_JSON", `{"compat":`, ""},
	} {
		_, _, _, err := EnvProvider(envMap(map[string]string{c.variable: c.json}))
		if err == nil || !strings.HasPrefix(err.Error(), "model configuration: "+c.variable) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s %s: %v", c.variable, c.json, err)
		}
	}
	// A value outside an enumeration names variable and field.
	_, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_PROVIDER_JSON": `{"compat":{"thinkingFormat":"qwen-chat-templat"}}`}))
	if err == nil || !strings.Contains(err.Error(), "AGW_LLM_PROVIDER_JSON") || !strings.Contains(err.Error(), "compat.thinkingFormat") {
		t.Fatalf("enumeration: %v", err)
	}
	// Variables out of range.
	for name, v := range map[string]string{"AGW_LLM_MAX_TOKENS": "300000", "AGW_LLM_CONTEXT_WINDOW": "abc", "AGW_LLM_THINKING": "maybe",
		"AGW_LLM_INPUT": "text,audio", "AGW_LLM_SAMPLING": "top_p", "AGW_LLM_BASE_URL": "ftp://x", "AGW_LLM_SELF_HOSTED": "vielleicht"} {
		if _, _, _, err := EnvProvider(envMap(map[string]string{name: v})); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: %v", name, v, err)
		}
	}
}

func TestEnvProviderReservedField(t *testing.T) {
	for _, c := range []struct{ variable, json, field, owner string }{
		{"AGW_LLM_MODEL_JSON", `{"input":["text"]}`, "input", "AGW_LLM_INPUT"},
		{"AGW_LLM_MODEL_JSON", `{"maxTokens":99}`, "maxTokens", "AGW_LLM_MAX_TOKENS"},
		{"AGW_LLM_MODEL_JSON", `{"id":"x"}`, "id", "AGW_LLM_MODEL_ID"},
		{"AGW_LLM_MODEL_JSON", `{"reasoning":true}`, "reasoning", "AGW_LLM_THINKING"},
		{"AGW_LLM_PROVIDER_JSON", `{"apiKey":"abc"}`, "apiKey", "AGW_LLM_API_KEY_ENV"},
		{"AGW_LLM_PROVIDER_JSON", `{"baseUrl":"http://x"}`, "baseUrl", "AGW_LLM_BASE_URL"},
		{"AGW_LLM_PROVIDER_JSON", `{"models":[]}`, "models", "AGW_LLM_MODEL_ID"},
		// Headers with a secret never go into the hashed catalog.
		{"AGW_LLM_PROVIDER_JSON", `{"headers":{"Authorization":"Bearer abc"}}`, "headers.Authorization", "secrets"},
		{"AGW_LLM_MODEL_JSON", `{"headers":{"x-api-key":"abc"}}`, "headers.x-api-key", "secrets"},
		// The proxy and the variables own these request fields.
		{"AGW_LLM_MODEL_JSON", `{"samplingParams":{"tags":["x"]}}`, "samplingParams.tags", "reserved"},
		{"AGW_LLM_MODEL_JSON", `{"samplingParams":{"temperature":0}}`, "samplingParams.temperature", "reserved"},
		{"AGW_LLM_MODEL_JSON", `{"samplingParams":{"chat_template_kwargs":{}}}`, "samplingParams.chat_template_kwargs", "reserved"},
	} {
		_, _, _, err := EnvProvider(envMap(map[string]string{c.variable: c.json}))
		if err == nil || !strings.Contains(err.Error(), c.variable) || !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), c.owner) {
			t.Errorf("%s %s: %v", c.variable, c.json, err)
		}
		if err != nil && (strings.Contains(err.Error(), "abc") && c.owner != "AGW_LLM_API_KEY_ENV") {
			t.Errorf("message echoes a value: %v", err)
		}
	}
}

func TestEnvProviderDuplicate(t *testing.T) {
	// thinkingLevelMap is generated from AGW_LLM_THINKING=on|off; only "user" takes the fragment.
	frag := `{"thinkingLevelMap":{"off":null}}`
	for _, th := range []string{"on", "off"} {
		_, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_THINKING": th, "AGW_LLM_MODEL_JSON": frag}))
		if err == nil || !strings.Contains(err.Error(), "thinkingLevelMap") || !strings.Contains(err.Error(), "twice") {
			t.Errorf("thinking=%s: %v", th, err)
		}
	}
	p, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_THINKING": "user", "AGW_LLM_MODEL_JSON": `{"thinkingLevelMap":{"off":null,"low":"low"}}`}))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := p.Models[0].ThinkingLevelMap["off"]; !ok || v != nil || *p.Models[0].ThinkingLevelMap["low"] != "low" {
		t.Fatalf("user map: %v", p.Models[0].ThinkingLevelMap)
	}
	// A fragment field that has its own variable is a duplicate too (reserved).
	if _, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_INPUT": "text", "AGW_LLM_MODEL_JSON": `{"input":["text","image"]}`})); err == nil {
		t.Fatal("input in both: no error")
	}
}

func TestEnvProviderMissingKey(t *testing.T) {
	_, _, _, err := EnvProvider(envMap(map[string]string{"FARM_API_KEY": "-"}))
	if err == nil || !strings.Contains(err.Error(), "AGW_LLM_API_KEY_ENV names FARM_API_KEY, but it is empty") || !strings.Contains(err.Error(), "secrets/agent-gateway.env") {
		t.Fatalf("missing key: %v", err)
	}
	// With the key set, no message of any error path may contain its value.
	for _, over := range []map[string]string{
		{"AGW_LLM_MODEL_JSON": `{"nonsense":1}`}, {"AGW_LLM_THINKING": "maybe"}, {"AGW_LLM_MAX_TOKENS": "0"}, {"AGW_LLM_MODEL_ID": "-"},
	} {
		if _, _, _, err := EnvProvider(envMap(over)); err == nil || strings.Contains(err.Error(), testKey) {
			t.Errorf("%v: %v", over, err)
		}
	}
}

func TestEnvProviderIDCollision(t *testing.T) {
	path := writeCatalogFile(t)
	_, err := LoadEffectiveCatalog(path, envMap(map[string]string{"AGW_LLM_PROVIDER": "deepseek", "AGW_LLM_BASE_URL": "-"}))
	if err == nil {
		t.Fatal("error expected")
	}
	// With a complete setting the collision itself is named.
	_, err = LoadEffectiveCatalog(path, envMap(map[string]string{"AGW_LLM_PROVIDER": "deepseek"}))
	if err == nil || !strings.Contains(err.Error(), "already in the catalog") {
		t.Fatalf("collision: %v", err)
	}
	if _, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_PROVIDER": "Farm_1"})); err == nil {
		t.Fatal("provider ID with upper case and underscore accepted")
	}
}

func TestAGWModelUnknown(t *testing.T) {
	path := writeCatalogFile(t)
	_, err := LoadEffectiveCatalog(path, envMap(map[string]string{"AGW_MODEL": "farm/does-not-exist"}))
	if err == nil || !strings.Contains(err.Error(), "AGW_MODEL") {
		t.Fatalf("unknown model: %v", err)
	}
	cat, err := LoadEffectiveCatalog(path, envMap(map[string]string{"AGW_MODEL": "farm/Org/Model-27B"}))
	if err != nil || cat.Default != "farm/Org/Model-27B" {
		t.Fatalf("default: %v %v", cat, err)
	}
	// Empty AGW_MODEL keeps the file's default; the older AGW_DEFAULT_MODEL still works; a conflict is refused.
	cat, err = LoadEffectiveCatalog(path, envMap(nil))
	if err != nil || cat.Default != "deepseek/deepseek-flash" {
		t.Fatalf("file default: %v %v", cat, err)
	}
	if _, err := LoadEffectiveCatalog(path, envMap(map[string]string{"AGW_MODEL": "farm/Org/Model-27B", "AGW_DEFAULT_MODEL": "deepseek/deepseek-flash"})); err == nil {
		t.Fatal("conflicting AGW_MODEL and AGW_DEFAULT_MODEL accepted")
	}
}

func TestExtraHostMustMatchBaseURL(t *testing.T) {
	if _, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_EXTRA_HOST": "other.example.internal:192.0.2.10"})); err == nil || !strings.Contains(err.Error(), "AGW_LLM_EXTRA_HOST") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, _, _, err := EnvProvider(envMap(map[string]string{"AGW_LLM_EXTRA_HOST": "llm.example.internal"})); err == nil {
		t.Fatal("host without ip accepted")
	}
}

func TestLookupSlashID(t *testing.T) {
	cat, err := LoadEffectiveCatalog(writeCatalogFile(t), envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	// The model ID of the provider contains a slash; Lookup cuts at the first one only.
	prov, m, ok := cat.Lookup("farm/Org/Model-27B")
	if !ok || prov.ID != "farm" || m.ID != "Org/Model-27B" {
		t.Fatalf("lookup: %v %v %v", prov.ID, m.ID, ok)
	}
	if _, _, ok := cat.Lookup("farm/Org"); ok {
		t.Fatal("prefix of the ID found")
	}
}

func TestEffectiveSHAChangesWithEnv(t *testing.T) {
	path := writeCatalogFile(t)
	sha := func(over map[string]string) string {
		t.Helper()
		cat, err := LoadEffectiveCatalog(path, envMap(over))
		if err != nil {
			t.Fatal(err)
		}
		s, err := cat.EffectiveSHA256()
		if err != nil || len(s) != 64 {
			t.Fatalf("sha: %q %v", s, err)
		}
		return s
	}
	base := sha(nil)
	if sha(nil) != base {
		t.Fatal("checksum not stable")
	}
	seen := map[string]string{base: "base"}
	for name, over := range map[string]map[string]string{
		"tag":        {"AGW_LLM_TAGS": "gb10"},
		"max_tokens": {"AGW_LLM_MAX_TOKENS": "8192"},
		"model":      {"AGW_LLM_MODEL_ID": "Org/Other"},
		"extra host": {"AGW_LLM_EXTRA_HOST": "llm.example.internal:192.0.2.10"},
		"thinking":   {"AGW_LLM_THINKING": "on"},
		"default":    {"AGW_MODEL": "farm/Org/Model-27B"},
		"url":        {"AGW_LLM_BASE_URL": "https://llm2.example.internal/v1"},
		"no env":     {"AGW_LLM_PROVIDER": "-", "AGW_LLM_BASE_URL": "-", "AGW_LLM_MODEL_ID": "-", "AGW_LLM_API_KEY_ENV": "-", "AGW_LLM_CONTEXT_WINDOW": "-", "AGW_LLM_MAX_TOKENS": "-"},
	} {
		s := sha(over)
		if prev, dup := seen[s]; dup {
			t.Errorf("%s has the same checksum as %s", name, prev)
		}
		seen[s] = name
	}
}

func TestEffectiveSHAExcludesKey(t *testing.T) {
	path := writeCatalogFile(t)
	a, _ := LoadEffectiveCatalog(path, envMap(nil))
	b, _ := LoadEffectiveCatalog(path, envMap(map[string]string{"FARM_API_KEY": "a-completely-different-key-value"}))
	sa, _ := a.EffectiveSHA256()
	sb, _ := b.EffectiveSHA256()
	if sa != sb {
		t.Fatal("the key value changes the checksum")
	}
	j, err := a.EffectiveJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), testKey) || !strings.Contains(string(j), "FARM_API_KEY") {
		t.Fatalf("effective JSON must name the variable and never the value: %s", j)
	}
	// Canonical: valid JSON with sorted keys.
	var v map[string]any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(j), `"catalog"`) > strings.Index(string(j), `"extra_host"`) {
		t.Fatalf("keys not sorted: %s", j)
	}
}

func TestPiModelsJSONEnv(t *testing.T) {
	over := map[string]string{
		"AGW_LLM_INPUT":         "text,image",
		"AGW_LLM_THINKING":      "on",
		"AGW_LLM_PROVIDER_JSON": `{"compat":{"thinkingFormat":"qwen-chat-template","maxTokensField":"max_tokens"},"headers":{"X-Group":"a"}}`,
		"AGW_LLM_MODEL_JSON":    `{"samplingParams":{"top_p":0.95},"compat":{"supportsStore":false},"cost":{"input":0.1,"output":0.2,"cacheRead":0,"cacheWrite":0}}`,
	}
	cat, err := LoadEffectiveCatalog(writeCatalogFile(t), envMap(over))
	if err != nil {
		t.Fatal(err)
	}
	b, err := cat.PiModelsJSON("http://orchestrator:18481")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testKey) {
		t.Fatal("key in the models.json for pi")
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string            `json:"baseUrl"`
			APIKey  string            `json:"apiKey"`
			Headers map[string]string `json:"headers"`
			Compat  map[string]any    `json:"compat"`
			Models  []map[string]any  `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	pr := doc.Providers["farm"]
	if pr.BaseURL != "http://orchestrator:18481/llm/farm" || pr.APIKey != "agw-proxy" || pr.Compat["thinkingFormat"] != "qwen-chat-template" || pr.Headers["X-Group"] != "a" {
		t.Fatalf("provider: %+v", pr)
	}
	m := pr.Models[0]
	if m["id"] != "Org/Model-27B" || m["reasoning"] != true || m["contextWindow"] != float64(262144) || m["maxTokens"] != float64(12288) {
		t.Fatalf("model: %v", m)
	}
	if in, _ := m["input"].([]any); len(in) != 2 || in[1] != "image" {
		t.Fatalf("input: %v", m["input"])
	}
	if sp, _ := m["samplingParams"].(map[string]any); sp["top_p"] != 0.95 {
		t.Fatalf("samplingParams: %v", m["samplingParams"])
	}
	if c, _ := m["compat"].(map[string]any); c["supportsStore"] != false {
		t.Fatalf("model compat: %v", m["compat"])
	}
	if c, _ := m["cost"].(map[string]any); c["output"] != 0.2 {
		t.Fatalf("cost: %v", m["cost"])
	}
	// thinking=on: pi may not offer "off" (key present, value null).
	lm, ok := m["thinkingLevelMap"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingLevelMap: %v", m["thinkingLevelMap"])
	}
	if v, has := lm["off"]; !has || v != nil || len(lm) != 1 {
		t.Fatalf("on: %v", lm)
	}
	// thinking=off: every level but off is null; user without a fragment: no map at all.
	cat, _ = LoadEffectiveCatalog(writeCatalogFile(t), envMap(map[string]string{"AGW_LLM_THINKING": "off"}))
	b, _ = cat.PiModelsJSON("http://o:1")
	if strings.Contains(string(b), `"off"`) || !strings.Contains(string(b), `"medium": null`) {
		t.Fatalf("off: %s", b)
	}
	cat, _ = LoadEffectiveCatalog(writeCatalogFile(t), envMap(map[string]string{"AGW_LLM_THINKING": "user"}))
	b, _ = cat.PiModelsJSON("http://o:1")
	if strings.Contains(string(b), "thinkingLevelMap") {
		t.Fatalf("user: %s", b)
	}
}

func TestPiSchemaMirrorVersion(t *testing.T) {
	// The struct mirror above follows one pi release. Whoever changes PI_VERSION has to re-read
	// dist/core/model-config.d.ts of the new release and change PiSchemaVersion with the structs.
	b, err := os.ReadFile(filepath.Join("..", "..", "images", "agw-basis", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ARG PI_VERSION="+PiSchemaVersion+"\n") {
		t.Fatalf("PI_VERSION in images/agw-basis/Dockerfile differs from PiSchemaVersion %s: update the mirror in envprovider.go", PiSchemaVersion)
	}
}

func writeCatalogFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(catalogJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

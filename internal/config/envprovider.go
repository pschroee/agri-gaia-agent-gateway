// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// One provider can be built from the environment (AGW_LLM_*), next to the static models.json. Which model
// the gateway uses is then switched in the platform's .env alone: a hosted API, a self-hosted
// OpenAI-compatible endpoint or a university service, without a code change or a second file. The endpoint
// of an internal service therefore never has to stand in this public repository.
//
// The two JSON fragments (AGW_LLM_PROVIDER_JSON, AGW_LLM_MODEL_JSON) carry the pi fields that have no
// variable of their own. pi checks models.json by type only and lets unknown fields pass silently, so a
// typo in "thinkingFormat" would simply have no effect. The fragments are therefore checked here, strictly,
// at start: an unknown field, a wrong type, a value outside the enumeration or a field that has its own
// variable stops the orchestrator with a message that names the variable and the field.
//
// Mirror of pi 0.87.1 dist/core/model-config.d.ts (ModelDefinitionSchema, ProviderConfigSchema, the
// "openai-completions" form of compat); update together with PI_VERSION in images/agw-basis/Dockerfile.
// TestPiSchemaMirrorVersion reminds whoever changes PI_VERSION.

// PiSchemaVersion is the pi version whose models.json schema the structs below mirror.
const PiSchemaVersion = "0.87.1"

// The variables of the environment provider.
const (
	VarModel        = "AGW_MODEL"
	VarProvider     = "AGW_LLM_PROVIDER"
	VarBaseURL      = "AGW_LLM_BASE_URL"
	VarModelID      = "AGW_LLM_MODEL_ID"
	VarAPIKeyEnv    = "AGW_LLM_API_KEY_ENV"
	VarAPI          = "AGW_LLM_API"
	VarContext      = "AGW_LLM_CONTEXT_WINDOW"
	VarMaxTokens    = "AGW_LLM_MAX_TOKENS"
	VarInput        = "AGW_LLM_INPUT"
	VarThinking     = "AGW_LLM_THINKING"
	VarTags         = "AGW_LLM_TAGS"
	VarSampling     = "AGW_LLM_SAMPLING"
	VarSelfHosted   = "AGW_LLM_SELF_HOSTED"
	VarInstanceHdrs = "AGW_LLM_INSTANCE_HEADERS"
	VarExtraHost    = "AGW_LLM_EXTRA_HOST"
	VarProviderJSON = "AGW_LLM_PROVIDER_JSON"
	VarModelJSON    = "AGW_LLM_MODEL_JSON"
)

var envProviderVars = []string{VarProvider, VarBaseURL, VarModelID, VarAPIKeyEnv, VarAPI, VarContext, VarMaxTokens, VarInput,
	VarThinking, VarTags, VarSampling, VarSelfHosted, VarInstanceHdrs, VarExtraHost, VarProviderJSON, VarModelJSON}

// defaultCompactReserve is the default of AGW_COMPACT_RESERVE_TOKENS; maxTokens must stay below the context
// window minus this reserve, or pi would compact too late for the answer to fit.
const defaultCompactReserve = 16384

var providerIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Fields of the model fragment that belong to a variable or to the gateway, and of the provider fragment.
var reservedModelFields = map[string]string{
	"id": "AGW_LLM_MODEL_ID", "api": "AGW_LLM_API", "baseUrl": "AGW_LLM_BASE_URL", "reasoning": "AGW_LLM_THINKING",
	"input": "AGW_LLM_INPUT", "contextWindow": "AGW_LLM_CONTEXT_WINDOW", "maxTokens": "AGW_LLM_MAX_TOKENS",
}
var reservedProviderFields = map[string]string{
	"baseUrl": "AGW_LLM_BASE_URL", "api": "AGW_LLM_API", "apiKey": "AGW_LLM_API_KEY_ENV (the gateway sets a placeholder; the key stays with the orchestrator)",
	"authHeader": "the gateway (the proxy sets the Authorization header)", "oauth": "the gateway", "models": "AGW_LLM_MODEL_ID and AGW_LLM_MODEL_JSON",
	"modelOverrides": "the gateway",
}

// samplingParams may not carry a field the proxy or the other variables own (one owner per field).
var reservedSamplingParams = []string{"tags", "temperature", "seed", "model", "messages", "stream", "tools",
	"max_tokens", "max_completion_tokens", "chat_template_kwargs"}

// Headers that carry a secret never go into the hashed, exported catalog.
var secretHeaders = []string{"authorization", "x-api-key", "api-key", "proxy-authorization"}

// piThinkingLevels are the thinking levels of pi's thinkingLevelMap.
var piThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// piCost: pi's cost object (USD per 1M tokens); required fields are pointers so a missing one is reported.
type piCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cacheRead"`
	CacheWrite *float64 `json:"cacheWrite"`
	Tiers      []any    `json:"tiers,omitempty"`
}

type piInputLimits struct {
	MaxRequestBytes *int64 `json:"maxRequestBytes,omitempty"`
	Images          *struct {
		Resize *struct {
			MaxWidth    *int64 `json:"maxWidth,omitempty"`
			MaxHeight   *int64 `json:"maxHeight,omitempty"`
			MaxBytes    *int64 `json:"maxBytes,omitempty"`
			JpegQuality *int64 `json:"jpegQuality,omitempty"`
		} `json:"resize,omitempty"`
		MaxPerMessage *int64 `json:"maxPerMessage,omitempty"`
		MaxPerRequest *int64 `json:"maxPerRequest,omitempty"`
	} `json:"images,omitempty"`
}

type piPromptCache struct {
	Short *float64 `json:"short,omitempty"`
	Long  *float64 `json:"long,omitempty"`
}

// chatTemplateValue: string | number | boolean | null | {"$var": "thinking.enabled"|"thinking.effort", "omitWhenOff": bool}.
type chatTemplateValue struct{ raw json.RawMessage }

func (v *chatTemplateValue) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return errors.New("empty value")
	}
	switch b[0] {
	case '{':
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var o struct {
			Var         string `json:"$var"`
			OmitWhenOff *bool  `json:"omitWhenOff"`
		}
		if err := dec.Decode(&o); err != nil {
			return err
		}
		if o.Var != "thinking.enabled" && o.Var != "thinking.effort" {
			return fmt.Errorf("$var must be thinking.enabled or thinking.effort")
		}
	case '[':
		return errors.New("an array is not allowed here")
	}
	v.raw = append(v.raw[:0], b...)
	return nil
}

func (v chatTemplateValue) MarshalJSON() ([]byte, error) {
	if len(v.raw) == 0 {
		return []byte("null"), nil
	}
	return v.raw, nil
}

// piCompatOpenAI: the "openai-completions" form of pi's compat object.
type piCompatOpenAI struct {
	SupportsStore                               *bool                        `json:"supportsStore,omitempty"`
	SupportsDeveloperRole                       *bool                        `json:"supportsDeveloperRole,omitempty"`
	SupportsReasoningEffort                     *bool                        `json:"supportsReasoningEffort,omitempty"`
	SupportsUsageInStreaming                    *bool                        `json:"supportsUsageInStreaming,omitempty"`
	SupportsFinishReason                        *bool                        `json:"supportsFinishReason,omitempty"`
	MaxTokensField                              *string                      `json:"maxTokensField,omitempty"`
	RequiresToolResultName                      *bool                        `json:"requiresToolResultName,omitempty"`
	RequiresAssistantAfterToolResult            *bool                        `json:"requiresAssistantAfterToolResult,omitempty"`
	RequiresThinkingAsText                      *bool                        `json:"requiresThinkingAsText,omitempty"`
	RequiresReasoningContentOnAssistantMessages *bool                        `json:"requiresReasoningContentOnAssistantMessages,omitempty"`
	ThinkingFormat                              *string                      `json:"thinkingFormat,omitempty"`
	ChatTemplateKwargs                          map[string]chatTemplateValue `json:"chatTemplateKwargs,omitempty"`
	ChatTemplateArgs                            map[string]chatTemplateValue `json:"chatTemplateArgs,omitempty"`
	CacheControlFormat                          *string                      `json:"cacheControlFormat,omitempty"`
	OpenRouterRouting                           map[string]json.RawMessage   `json:"openRouterRouting,omitempty"`
	VercelGatewayRouting                        *struct {
		Only  []string `json:"only,omitempty"`
		Order []string `json:"order,omitempty"`
	} `json:"vercelGatewayRouting,omitempty"`
	SupportsOpenAIGrammarTools *bool    `json:"supportsOpenAIGrammarTools,omitempty"`
	SupportsStrictMode         *bool    `json:"supportsStrictMode,omitempty"`
	SendSessionAffinityHeaders *bool    `json:"sendSessionAffinityHeaders,omitempty"`
	SessionAffinityFormat      *string  `json:"sessionAffinityFormat,omitempty"`
	SupportsLongCacheRetention *bool    `json:"supportsLongCacheRetention,omitempty"`
	VllmPriority               *float64 `json:"vllmPriority,omitempty"`
}

var openRouterRoutingKeys = map[string]bool{"allow_fallbacks": true, "require_parameters": true, "data_collection": true,
	"zdr": true, "enforce_distillable_text": true, "order": true, "only": true, "ignore": true, "quantizations": true,
	"sort": true, "max_price": true, "preferred_min_throughput": true, "preferred_max_latency": true}

func oneOf(v *string, allowed ...string) error {
	if v == nil {
		return nil
	}
	for _, a := range allowed {
		if *v == a {
			return nil
		}
	}
	return fmt.Errorf("must be one of %s", strings.Join(allowed, ", "))
}

func (c *piCompatOpenAI) validate() error {
	if c == nil {
		return nil
	}
	checks := []struct {
		field string
		err   error
	}{
		{"maxTokensField", oneOf(c.MaxTokensField, "max_completion_tokens", "max_tokens")},
		{"thinkingFormat", oneOf(c.ThinkingFormat, "openai", "openrouter", "together", "baseten", "deepseek", "zai", "qwen",
			"chat-template", "qwen-chat-template", "string-thinking", "ant-ling")},
		{"cacheControlFormat", oneOf(c.CacheControlFormat, "anthropic")},
		{"sessionAffinityFormat", oneOf(c.SessionAffinityFormat, "openai", "openai-nosession", "openrouter")},
	}
	for _, ck := range checks {
		if ck.err != nil {
			return fmt.Errorf("compat.%s: %w", ck.field, ck.err)
		}
	}
	for k := range c.OpenRouterRouting {
		if !openRouterRoutingKeys[k] {
			return fmt.Errorf("compat.openRouterRouting.%s: unknown field", k)
		}
	}
	return nil
}

// piProviderFragment and piModelFragment: what AGW_LLM_PROVIDER_JSON and AGW_LLM_MODEL_JSON may carry.
type piProviderFragment struct {
	Name    *string           `json:"name,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Compat  *piCompatOpenAI   `json:"compat,omitempty"`
}

type piModelFragment struct {
	Name             *string            `json:"name,omitempty"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap,omitempty"`
	InputLimits      *piInputLimits     `json:"inputLimits,omitempty"`
	Cost             *piCost            `json:"cost,omitempty"`
	PromptCache      *piPromptCache     `json:"promptCache,omitempty"`
	SamplingParams   map[string]any     `json:"samplingParams,omitempty"`
	Headers          map[string]string  `json:"headers,omitempty"`
	Compat           *piCompatOpenAI    `json:"compat,omitempty"`
}

func modelErr(variable, field, problem string) error {
	if field == "" {
		return fmt.Errorf("model configuration: %s: %s", variable, problem)
	}
	return fmt.Errorf("model configuration: %s: %s: %s", variable, field, problem)
}

// decodeFragment decodes raw strictly into dst. Reserved fields (they have a variable of their own) are
// named as such; everything else unknown, or of the wrong type, is reported with the variable and the field.
// Values are never part of a message: the key stays out of every error text.
func decodeFragment(variable, raw string, reserved map[string]string, dst any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return modelErr(variable, "", "not a JSON object ("+jsonErrKind(err)+")")
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if owner, ok := reserved[k]; ok {
			return modelErr(variable, k, "reserved, set it with "+owner)
		}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &te):
			return modelErr(variable, te.Field, "wrong type, expected "+te.Type.String()+" but got "+te.Value)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return modelErr(variable, strings.TrimPrefix(err.Error(), "json: unknown field "), "unknown field")
		}
		return modelErr(variable, "", jsonErrKind(err))
	}
	return nil
}

// jsonErrKind describes a decoding error without echoing the input.
func jsonErrKind(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return "syntax error at offset " + strconv.FormatInt(se.Offset, 10)
	}
	return "invalid JSON"
}

func checkHeaders(variable string, h map[string]string) error {
	for k := range h {
		for _, s := range secretHeaders {
			if strings.EqualFold(k, s) {
				return modelErr(variable, "headers."+k, "secrets never go into the catalog; the key comes from AGW_LLM_API_KEY_ENV")
			}
		}
	}
	return nil
}

func parseCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIntVar(getenv func(string) string, name string, required bool) (int, error) {
	s := strings.TrimSpace(getenv(name))
	if s == "" {
		if required {
			return 0, modelErr(name, "", "required")
		}
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, modelErr(name, "", "must be a positive integer")
	}
	return n, nil
}

// EnvProvider builds the provider of the environment. It returns nil when AGW_LLM_PROVIDER is empty. The
// second result is AGW_MODEL (the default model, empty = the catalog file's), the third the host mapping for
// Compose ("name:ip", empty = none). A key that is named but empty, an unknown or mistyped field in a
// fragment, a duplicated setting or a value out of range is an error; the text names the variable and the
// field, never a value.
func EnvProvider(getenv func(string) string) (*Provider, string, string, error) {
	agwModel := strings.TrimSpace(getenv(VarModel))
	extra := strings.TrimSpace(getenv(VarExtraHost))
	id := strings.TrimSpace(getenv(VarProvider))
	if id == "" {
		// Settings without a provider would be ignored silently: refuse them.
		for _, v := range envProviderVars {
			if strings.TrimSpace(getenv(v)) != "" {
				return nil, "", "", modelErr(v, "", VarProvider+" is empty, so the variable would have no effect")
			}
		}
		return nil, agwModel, "", nil
	}
	if !providerIDRe.MatchString(id) {
		return nil, "", "", modelErr(VarProvider, "", "must match [a-z0-9-]+")
	}
	need := func(name string) (string, error) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return "", modelErr(name, "", "required when "+VarProvider+" is set")
		}
		return v, nil
	}
	base, err := need(VarBaseURL)
	if err != nil {
		return nil, "", "", err
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", "", modelErr(VarBaseURL, "", "must be an http or https URL")
	}
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/chat/completions") {
		return nil, "", "", modelErr(VarBaseURL, "", "must end before /chat/completions (the proxy appends the path)")
	}
	modelID, err := need(VarModelID)
	if err != nil {
		return nil, "", "", err
	}
	keyEnv, err := need(VarAPIKeyEnv)
	if err != nil {
		return nil, "", "", err
	}
	if getenv(keyEnv) == "" {
		return nil, "", "", fmt.Errorf("model configuration: %s names %s, but it is empty in the orchestrator's environment (secrets/agent-gateway.env)", VarAPIKeyEnv, keyEnv)
	}
	api := strings.TrimSpace(getenv(VarAPI))
	if api == "" {
		api = "openai-completions"
	}
	if _, ok := allowedAPIs[api]; !ok {
		return nil, "", "", modelErr(VarAPI, "", "must be openai-completions, openai-responses or anthropic-messages")
	}
	ctxWin, err := parseIntVar(getenv, VarContext, true)
	if err != nil {
		return nil, "", "", err
	}
	maxTok, err := parseIntVar(getenv, VarMaxTokens, true)
	if err != nil {
		return nil, "", "", err
	}
	reserve := defaultCompactReserve
	if s := strings.TrimSpace(getenv("AGW_COMPACT_RESERVE_TOKENS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			reserve = n
		}
	}
	if maxTok >= ctxWin-reserve {
		return nil, "", "", modelErr(VarMaxTokens, "", fmt.Sprintf("must be below the context window minus the compaction reserve (%d - %d)", ctxWin, reserve))
	}
	input := []string{"text"}
	if s := strings.TrimSpace(getenv(VarInput)); s != "" {
		input = parseCSV(s)
		for _, in := range input {
			if in != "text" && in != "image" {
				return nil, "", "", modelErr(VarInput, "", "each entry must be text or image")
			}
		}
	}
	thinking := strings.TrimSpace(getenv(VarThinking))
	if thinking == "" {
		thinking = "user"
	}
	if thinking != "on" && thinking != "off" && thinking != "user" {
		return nil, "", "", modelErr(VarThinking, "", "must be on, off or user")
	}
	var sampling *Sampling
	if s := strings.TrimSpace(getenv(VarSampling)); s != "" {
		sampling = &Sampling{}
		for _, f := range parseCSV(s) {
			switch f {
			case "temperature":
				sampling.Temperature = true
			case "seed":
				sampling.Seed = true
			default:
				return nil, "", "", modelErr(VarSampling, "", "each entry must be temperature or seed")
			}
		}
	}
	selfHosted := false
	switch strings.ToLower(strings.TrimSpace(getenv(VarSelfHosted))) {
	case "", "false", "0":
	case "true", "1":
		selfHosted = true
	default:
		return nil, "", "", modelErr(VarSelfHosted, "", "must be true or false")
	}
	if extra != "" {
		name, ip, ok := strings.Cut(extra, ":")
		if !ok || name == "" || ip == "" {
			return nil, "", "", modelErr(VarExtraHost, "", "must have the form name:ip")
		}
		if !strings.EqualFold(name, u.Hostname()) {
			return nil, "", "", modelErr(VarExtraHost, "", "the host name must be the host of "+VarBaseURL)
		}
	}

	var pf piProviderFragment
	if err := decodeFragment(VarProviderJSON, getenv(VarProviderJSON), reservedProviderFields, &pf); err != nil {
		return nil, "", "", err
	}
	var mf piModelFragment
	if err := decodeFragment(VarModelJSON, getenv(VarModelJSON), reservedModelFields, &mf); err != nil {
		return nil, "", "", err
	}
	if api != "openai-completions" && (pf.Compat != nil || mf.Compat != nil) {
		return nil, "", "", modelErr(VarProviderJSON, "compat", "compat is only checked and passed on for openai-completions")
	}
	if err := pf.Compat.validate(); err != nil {
		return nil, "", "", modelErr(VarProviderJSON, "", err.Error())
	}
	if err := mf.Compat.validate(); err != nil {
		return nil, "", "", modelErr(VarModelJSON, "", err.Error())
	}
	if err := checkHeaders(VarProviderJSON, pf.Headers); err != nil {
		return nil, "", "", err
	}
	if err := checkHeaders(VarModelJSON, mf.Headers); err != nil {
		return nil, "", "", err
	}
	for _, k := range reservedSamplingParams {
		if _, ok := mf.SamplingParams[k]; ok {
			return nil, "", "", modelErr(VarModelJSON, "samplingParams."+k, "reserved; the gateway or another variable sets this field")
		}
	}
	for k := range mf.ThinkingLevelMap {
		found := false
		for _, l := range piThinkingLevels {
			found = found || k == l
		}
		if !found {
			return nil, "", "", modelErr(VarModelJSON, "thinkingLevelMap."+k, "unknown thinking level")
		}
	}
	// One owner per setting: thinkingLevelMap is generated from AGW_LLM_THINKING unless that is "user".
	var levelMap map[string]*string
	switch thinking {
	case "on":
		if mf.ThinkingLevelMap != nil {
			return nil, "", "", modelErr(VarModelJSON, "thinkingLevelMap", "set twice: "+VarThinking+"=on generates it; use "+VarThinking+"=user to give your own")
		}
		levelMap = map[string]*string{"off": nil}
	case "off":
		if mf.ThinkingLevelMap != nil {
			return nil, "", "", modelErr(VarModelJSON, "thinkingLevelMap", "set twice: "+VarThinking+"=off generates it; use "+VarThinking+"=user to give your own")
		}
		levelMap = map[string]*string{}
		for _, l := range piThinkingLevels {
			if l != "off" {
				levelMap[l] = nil
			}
		}
	default:
		levelMap = mf.ThinkingLevelMap
	}
	if mf.Cost != nil {
		if mf.Cost.Input == nil || mf.Cost.Output == nil || mf.Cost.CacheRead == nil || mf.Cost.CacheWrite == nil {
			return nil, "", "", modelErr(VarModelJSON, "cost", "input, output, cacheRead and cacheWrite are required")
		}
		if len(mf.Cost.Tiers) > 0 {
			return nil, "", "", modelErr(VarModelJSON, "cost.tiers", "not supported: the gateway prices a call at one flat price")
		}
	}

	m := Model{
		ID:               modelID,
		Reasoning:        true,
		ContextWindow:    ctxWin,
		MaxTokens:        maxTok,
		Input:            input,
		SelfHosted:       selfHosted,
		Sampling:         sampling,
		ThinkingLevelMap: levelMap,
		RequestTags:      parseCSV(getenv(VarTags)),
		InstanceHeaders:  parseCSV(getenv(VarInstanceHdrs)),
		Thinking:         thinking,
	}
	if mf.Name != nil {
		m.Name = *mf.Name
	}
	if mf.Cost != nil {
		m.Pricing = &Pricing{Input: *mf.Cost.Input, Output: *mf.Cost.Output, CacheRead: *mf.Cost.CacheRead,
			CacheWrite: *mf.Cost.CacheWrite, Currency: "USD", Note: "from " + VarModelJSON}
	}
	m.Headers = mf.Headers
	if mf.Compat != nil {
		m.Compat, _ = json.Marshal(mf.Compat)
	}
	extraFields := map[string]any{}
	if mf.InputLimits != nil {
		extraFields["inputLimits"] = mf.InputLimits
	}
	if mf.PromptCache != nil {
		extraFields["promptCache"] = mf.PromptCache
	}
	if mf.SamplingParams != nil {
		extraFields["samplingParams"] = mf.SamplingParams
	}
	if len(extraFields) > 0 {
		m.PiExtra, _ = json.Marshal(extraFields) // map keys come out sorted
	}

	p := &Provider{ID: id, Upstream: strings.TrimRight(base, "/"), API: api, APIKeyEnv: keyEnv, Headers: pf.Headers, Models: []Model{m}}
	if pf.Name != nil {
		p.DisplayName = *pf.Name
	}
	if pf.Compat != nil {
		p.Compat, _ = json.Marshal(pf.Compat)
	}
	return p, agwModel, extra, nil
}

var allowedAPIs = map[string]struct{}{"openai-completions": {}, "openai-responses": {}, "anthropic-messages": {}}

// ThinkingFormat reports the thinkingFormat of the model's compat, falling back to the provider's.
func (p Provider) ThinkingFormat(m Model) string {
	for _, raw := range []json.RawMessage{m.Compat, p.Compat} {
		if len(raw) == 0 {
			continue
		}
		var c struct {
			ThinkingFormat string `json:"thinkingFormat"`
		}
		if json.Unmarshal(raw, &c) == nil && c.ThinkingFormat != "" {
			return c.ThinkingFormat
		}
	}
	return ""
}

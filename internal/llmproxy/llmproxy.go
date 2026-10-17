// Package llmproxy forwards the sandboxes' model calls to the provider.
// The key stays with the orchestrator; only POST calls with a model from
// the catalog are let through.
package llmproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"agw/internal/config"
)

const (
	maxBody       = 16 << 20
	maxConcurrent = 32
)

// allowedPaths are the endpoints per API kind; everything else (files, batches,
// fine-tuning …) stays closed to the agent.
var allowedPaths = map[string][]string{
	"openai-completions": {"chat/completions"},
	"openai-responses":   {"responses"},
	"anthropic-messages": {"v1/messages", "messages"},
}

type Proxy struct {
	cat *config.Catalog
	sem chan struct{}
	rec Recorder

	mu       sync.Mutex
	inFlight map[string]int // per chat
}

// SetRecorder turns on attribution and billing. Without a recorder (tests)
// nothing is recorded and the origin is not checked.
func (p *Proxy) SetRecorder(r Recorder) { p.rec = r }

func New(cat *config.Catalog) *Proxy {
	return &Proxy{cat: cat, sem: make(chan struct{}, maxConcurrent), inFlight: map[string]int{}}
}

// enter counts a running call of the chat; false if the limit is reached.
func (p *Proxy) enter(chatID string, max int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if max > 0 && p.inFlight[chatID] >= max {
		return false
	}
	p.inFlight[chatID]++
	return true
}

func (p *Proxy) leave(chatID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[chatID]--; p.inFlight[chatID] <= 0 {
		delete(p.inFlight, chatID)
	}
}

// modelOf reads the model strictly: exactly one key that, ignoring case,
// is called "model", spelled exactly like that and a
// string. encoding/json otherwise matches keys leniently (last match
// wins), the provider exactly – that would allow bypassing the list.
func modelOf(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return "", errors.New("request is not a JSON object")
	}
	model, found := "", 0
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", err
		}
		key, _ := kt.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return "", err
		}
		if !strings.EqualFold(key, "model") {
			continue
		}
		found++
		if key != "model" || json.Unmarshal(val, &model) != nil {
			return "", errors.New("field model ambiguous or not a string")
		}
	}
	if found != 1 {
		return "", errors.New("field model missing or present more than once")
	}
	return model, nil
}

// toolsetOf extracts the tools of a request body and returns their canonical form (keys sorted by encoding/json,
// which sorts map keys) with its SHA-256 (X5.6). ok is false when the request carries no tools list.
func toolsetOf(body []byte) (sha string, tools json.RawMessage, ok bool) {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return "", nil, false
	}
	raw, found := req["tools"]
	if !found {
		return "", nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", nil, false
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", nil, false
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), canon, true
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "agw-proxy: " + msg}})
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/llm/")
	if !ok {
		deny(w, http.StatusNotFound, "unknown path")
		return
	}
	provID, subPath, _ := strings.Cut(rest, "/")
	prov, ok := p.cat.Provider(provID)
	if !ok {
		deny(w, http.StatusNotFound, "unknown provider "+provID)
		return
	}
	if r.Method != http.MethodPost {
		deny(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var chatID, slotID, srcIP string
	var att Attribution
	if p.rec != nil {
		srcIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		att = p.rec.Attribute(srcIP)
		chatID, slotID = att.ChatID, att.SlotID
		if chatID == "" {
			slog.Warn("LLM proxy: call without an assigned slot refused", "from", srcIP)
			deny(w, http.StatusForbidden, "call cannot be attributed to a chat")
			return
		}
	}
	pathOK := false
	for _, a := range allowedPaths[prov.API] {
		if subPath == a {
			pathOK = true
		}
	}
	if !pathOK {
		slog.Warn("LLM proxy: path refused", "provider", provID, "path", subPath)
		deny(w, http.StatusForbidden, "path not allowed: "+subPath)
		return
	}
	if p.rec != nil {
		if !p.enter(chatID, att.MaxConcurrent) {
			slog.Warn("LLM proxy: limit of concurrent agents reached", "chat", chatID, "limit", att.MaxConcurrent)
			p.rec.LimitHit(chatID, att.MaxConcurrent)
			deny(w, http.StatusTooManyRequests, "limit of concurrent agents of this chat reached (at most "+strconv.Itoa(att.MaxConcurrent)+")")
			return
		}
		defer p.leave(chatID)
		// The model-cost budget of an autonomous plan (A-E1, D1.9) or the limits of an experiment run (X3) are
		// checked before the call is forwarded.
		if att.Budget != nil {
			if err := att.Budget(chatID); err != nil {
				slog.Warn("LLM proxy: budget refused the call", "chat", chatID, "reason", err)
				deny(w, http.StatusTooManyRequests, err.Error())
				return
			}
		}
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		deny(w, http.StatusTooManyRequests, "too many concurrent requests")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		deny(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	model, err := modelOf(body)
	if err != nil {
		slog.Warn("LLM proxy: request refused", "provider", provID, "reason", err)
		deny(w, http.StatusBadRequest, err.Error())
		return
	}
	// X3.1: an experiment run accepts only the chat's model, not just any model of the catalog (the title
	// model included, which is why titles are off for experiment chats).
	modelID := provID + "/" + model
	_, mcfg, known := p.cat.Lookup(modelID)
	if att.Model != "" && modelID != att.Model {
		slog.Warn("LLM proxy: model refused in experiment run", "chat", chatID, "model", model)
		deny(w, http.StatusForbidden, "model not allowed in this experiment run: "+model)
		return
	}
	if !known {
		slog.Warn("LLM proxy: model refused", "provider", provID, "model", model)
		deny(w, http.StatusForbidden, "model not allowed: "+model)
		return
	}
	// X3.6: for an experiment run the proxy sets temperature and seed, but only where the catalog says the
	// model takes them; the values actually sent are recorded with the call.
	var sentTemp *float64
	var sentSeed *int64
	if att.Experiment {
		if mcfg.TakesTemperature() {
			t := att.Temperature
			sentTemp = &t
		}
		if mcfg.TakesSeed() && att.Seed != 0 {
			s := att.Seed
			sentSeed = &s
		}
	}
	if sentTemp != nil || sentSeed != nil {
		rewritten, err := setSampling(body, sentTemp, sentSeed)
		if err != nil {
			slog.Warn("LLM proxy: request refused", "provider", provID, "reason", err)
			deny(w, http.StatusBadRequest, err.Error())
			return
		}
		body = rewritten
	}
	// X5.6: the tool list of the request as sent (the sampling fields do not touch it).
	toolsSHA, toolsJSON, hasTools := toolsetOf(body)
	target, err := url.Parse(prov.Upstream)
	if err != nil {
		deny(w, http.StatusBadGateway, "upstream address invalid")
		return
	}
	started := time.Now()
	rp := &httputil.ReverseProxy{
		ModifyResponse: func(resp *http.Response) error {
			if p.rec == nil {
				return nil
			}
			// Issue #151: the proxy reads every response to meter tokens. A compressed body would pass
			// through it unreadable and the call would be recorded with zeros while pi decodes it. The
			// outgoing request asks for no encoding, so the transport usually decompresses already; this
			// covers an upstream that compresses anyway.
			if err := decompress(resp); err != nil {
				slog.Error("LLM proxy: compressed response not readable", "provider", provID, "error", err)
				return err
			}
			m := newMeter(prov.API)
			status := resp.StatusCode
			resp.Body = &meteredBody{ReadCloser: resp.Body, m: m, done: func(c Call) {
				c.ChatID, c.SlotID, c.SourceIP, c.Status = chatID, slotID, srcIP, status
				c.Model = provID + "/" + model
				c.Temperature, c.Seed = sentTemp, sentSeed
				c.StartedAt, c.Duration = started, time.Since(started)
				c.DurationMs = c.Duration.Milliseconds()
				c.RequestBytes = int64(len(body))
				if hasTools {
					c.ToolsSHA256, c.Tools = toolsSHA, toolsJSON
				}
				if cost, peak, ok := p.cat.Cost(c.Model, c.Usage, started); ok {
					c.Cost, c.Peak = cost, peak
				}
				p.rec.Record(c)
			}}
			return nil
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + "/" + subPath
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			// Issue #151: no Accept-Encoding from the sandbox. The Go transport then negotiates gzip itself
			// and decompresses the answer transparently, so the meter sees the plain SSE/JSON. Forwarding
			// the sandbox's header would leave the body compressed here.
			pr.Out.Header.Del("Accept-Encoding")
			if key := prov.APIKey(); key != "" {
				if prov.API == "anthropic-messages" {
					pr.Out.Header.Set("X-Api-Key", key)
				} else {
					pr.Out.Header.Set("Authorization", "Bearer "+key)
				}
			}
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Error("LLM proxy: upstream error", "provider", provID, "error", err)
			deny(w, http.StatusBadGateway, "upstream not reachable")
		},
	}
	rp.ServeHTTP(w, r)
}

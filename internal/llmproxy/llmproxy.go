// Package llmproxy leitet die Modellaufrufe der Sandboxen an den Anbieter
// weiter. Der Schlüssel bleibt beim Orchestrator; durchgelassen werden nur
// POST-Aufrufe mit einem Modell aus dem Katalog.
package llmproxy

import (
	"bytes"
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

// allowedPaths sind die Endpunkte je API-Art; alles andere (Files, Batches,
// Fine-Tuning …) bleibt dem Agenten verschlossen.
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
	inFlight map[string]int // je Chat
}

// SetRecorder schaltet Zuordnung und Abrechnung ein. Ohne Recorder (Tests)
// wird nichts erfasst und nicht nach Herkunft geprüft.
func (p *Proxy) SetRecorder(r Recorder) { p.rec = r }

func New(cat *config.Catalog) *Proxy {
	return &Proxy{cat: cat, sem: make(chan struct{}, maxConcurrent), inFlight: map[string]int{}}
}

// enter zählt einen laufenden Aufruf des Chats; false, wenn die Grenze erreicht ist.
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

// modelOf liest das Modell streng: genau ein Schlüssel, der ohne Rücksicht
// auf Groß-/Kleinschreibung "model" heißt, exakt so geschrieben und als
// String. encoding/json ordnet Schlüssel sonst tolerant zu (letzter Treffer
// gewinnt), der Anbieter aber exakt – so ließe sich die Liste umgehen.
func modelOf(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return "", errors.New("Anfrage ist kein JSON-Objekt")
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
			return "", errors.New("Feld model mehrdeutig oder kein String")
		}
	}
	if found != 1 {
		return "", errors.New("Feld model fehlt oder ist mehrfach vorhanden")
	}
	return model, nil
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "agw-proxy: " + msg}})
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/llm/")
	if !ok {
		deny(w, http.StatusNotFound, "unbekannter Pfad")
		return
	}
	provID, subPath, _ := strings.Cut(rest, "/")
	prov, ok := p.cat.Provider(provID)
	if !ok {
		deny(w, http.StatusNotFound, "unbekannter Anbieter "+provID)
		return
	}
	if r.Method != http.MethodPost {
		deny(w, http.StatusMethodNotAllowed, "nur POST")
		return
	}
	var chatID, slotID, srcIP string
	var att Attribution
	if p.rec != nil {
		srcIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		att = p.rec.Attribute(srcIP)
		chatID, slotID = att.ChatID, att.SlotID
		if chatID == "" {
			slog.Warn("LLM-Proxy: Aufruf ohne zugewiesenen Platz abgewiesen", "von", srcIP)
			deny(w, http.StatusForbidden, "Aufruf keinem Chat zuzuordnen")
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
		slog.Warn("LLM-Proxy: Pfad abgewiesen", "anbieter", provID, "pfad", subPath)
		deny(w, http.StatusForbidden, "Pfad nicht freigegeben: "+subPath)
		return
	}
	if p.rec != nil {
		if !p.enter(chatID, att.MaxConcurrent) {
			slog.Warn("LLM-Proxy: Grenze gleichzeitiger Agenten erreicht", "chat", chatID, "grenze", att.MaxConcurrent)
			p.rec.LimitHit(chatID, att.MaxConcurrent)
			deny(w, http.StatusTooManyRequests, "Grenze gleichzeitiger Agenten dieses Chats erreicht (höchstens "+strconv.Itoa(att.MaxConcurrent)+")")
			return
		}
		defer p.leave(chatID)
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		deny(w, http.StatusTooManyRequests, "zu viele gleichzeitige Anfragen")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		deny(w, http.StatusRequestEntityTooLarge, "Anfrage zu groß")
		return
	}
	model, err := modelOf(body)
	if err != nil {
		slog.Warn("LLM-Proxy: Anfrage abgewiesen", "anbieter", provID, "grund", err)
		deny(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, _, ok := p.cat.Lookup(provID + "/" + model); !ok {
		slog.Warn("LLM-Proxy: Modell abgewiesen", "anbieter", provID, "modell", model)
		deny(w, http.StatusForbidden, "Modell nicht freigegeben: "+model)
		return
	}
	target, err := url.Parse(prov.Upstream)
	if err != nil {
		deny(w, http.StatusBadGateway, "Upstream-Adresse ungültig")
		return
	}
	started := time.Now()
	rp := &httputil.ReverseProxy{
		ModifyResponse: func(resp *http.Response) error {
			if p.rec == nil {
				return nil
			}
			m := newMeter(prov.API)
			status := resp.StatusCode
			resp.Body = &meteredBody{ReadCloser: resp.Body, m: m, done: func(c Call) {
				c.ChatID, c.SlotID, c.SourceIP, c.Status = chatID, slotID, srcIP, status
				c.Model = provID + "/" + model
				c.StartedAt, c.Duration = started, time.Since(started)
				c.DurationMs = c.Duration.Milliseconds()
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
			slog.Error("LLM-Proxy: Upstream-Fehler", "anbieter", provID, "fehler", err)
			deny(w, http.StatusBadGateway, "Upstream nicht erreichbar")
		},
	}
	rp.ServeHTTP(w, r)
}

package llmproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"agw/internal/config"
)

// Call ist ein abgerechneter Modellaufruf. Er wird am Proxy erfasst und
// ist damit unabhängig von dem, was pi oder die Sandbox melden: Hier tauchen
// auch Subagenten, Kompaktierungen und direkte Aufrufe aus der Sandbox auf.
type Call struct {
	ChatID     string       `json:"chat_id"`
	SlotID     string       `json:"slot_id"`
	SourceIP   string       `json:"source_ip"`
	Model      string       `json:"model"` // anbieter/modell
	ResponseID string       `json:"response_id"`
	Status     int          `json:"status"`
	Usage      config.Usage `json:"usage"`
	Cost       float64      `json:"cost"`
	Peak       bool         `json:"peak"`
	ToolCalls  []ToolCall   `json:"tool_calls"`
	// FinishReason des Anbieters (stop, tool_calls, length, …; Anthropic stop_reason).
	FinishReason string `json:"finish_reason"`
	// Complete: Die Antwort kam vollständig an (SSE mit finish_reason bzw. message_stop, JSON
	// lesbar). Werkzeugaufrufe aus einer abgebrochenen Antwort führt pi nicht aus (M1).
	Complete   bool          `json:"complete"`
	StartedAt  time.Time     `json:"started_at"`
	Duration   time.Duration `json:"-"`
	DurationMs int64         `json:"duration_ms"`
}

// ToolCall ist ein vom Modell angeforderter Werkzeugaufruf. ID ist die
// Kennung des Anbieters (tool_calls[].id); pi gibt sie als toolCallId an die
// Werkzeuge weiter, und der Abgleich mit tool_executions läuft über sie (E9).
type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Attribution ordnet eine Quelladresse einem Chat zu.
type Attribution struct {
	ChatID string
	SlotID string
	// MaxConcurrent: höchstens so viele Modellaufrufe dieses Chats zugleich
	// (Hauptagent plus erlaubte Subagenten). Harte Grenze, weil sie am Proxy
	// außerhalb der Sandbox gilt. 0: keine Grenze.
	MaxConcurrent int
}

// Recorder ordnet Aufrufe Chats zu und speichert sie.
type Recorder interface {
	// Attribute: leerer Chat heißt kein zugewiesener Platz, der Aufruf wird abgewiesen.
	Attribute(remoteIP string) Attribution
	Record(c Call)
	// LimitHit meldet einen wegen der Grenze abgewiesenen Aufruf.
	LimitHit(chatID string, max int)
}

// meter liest die Antwort des Anbieters mit, während sie weitergereicht wird
// (SSE oder JSON), und sammelt Antwort-ID, Tokens und Werkzeugaufrufe.
type meter struct {
	api     string
	mu      sync.Mutex
	buf     bytes.Buffer // unvollständige SSE-Zeile bzw. JSON-Rumpf
	sse     bool
	decided bool
	id      string
	usage   *rawUsage
	tools   map[int]*ToolCall
	order   []int
	byID    map[string]int // id des Aufrufs → Schlüssel in tools (Stücke ohne index, L9)
	last    int            // zuletzt begonnener Aufruf
	nextKey int            // Schlüssel für Aufrufe ohne index (negativ, damit sie keinen index treffen)
	finish  string
	stopped bool // Anthropic message_stop
	jsonOK  bool // ganze JSON-Antwort gelesen
}

type rawUsage struct {
	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// Anthropic
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

const maxMeterJSON = 16 << 20

func newMeter(api string) *meter {
	return &meter{api: api, tools: map[int]*ToolCall{}, byID: map[string]int{}, last: -1 << 30, nextKey: -1}
}

func (m *meter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.decided {
		trim := bytes.TrimLeft(p, " \r\n\t")
		if len(trim) > 0 {
			m.decided = true
			m.sse = !bytes.HasPrefix(trim, []byte("{"))
		}
	}
	if m.buf.Len()+len(p) > maxMeterJSON && !m.sse {
		return len(p), nil // übergroße JSON-Antwort: nicht auswerten
	}
	m.buf.Write(p)
	if m.sse {
		for {
			line, err := m.buf.ReadBytes('\n')
			if err != nil {
				m.buf.Reset()
				m.buf.Write(line) // Rest für den nächsten Schreibvorgang
				break
			}
			m.sseLine(bytes.TrimRight(line, "\r\n"))
		}
	}
	return len(p), nil
}

func (m *meter) sseLine(line []byte) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	m.chunk(data)
}

// chunk wertet ein JSON-Stück aus (SSE-Ereignis oder ganze Antwort).
func (m *meter) chunk(data []byte) {
	var c struct {
		ID      string    `json:"id"`
		Usage   *rawUsage `json:"usage"`
		Choices []struct {
			Delta        *msgPart `json:"delta"`
			Message      *msgPart `json:"message"`
			FinishReason *string  `json:"finish_reason"`
		} `json:"choices"`
		// Anthropic
		Type    string `json:"type"`
		Message *struct {
			ID    string    `json:"id"`
			Usage *rawUsage `json:"usage"`
		} `json:"message"`
		AnthropicDelta *struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if !m.sse {
		m.jsonOK = true
	}
	if c.Type == "message_stop" {
		m.stopped = true
	}
	if c.AnthropicDelta != nil && c.AnthropicDelta.StopReason != "" {
		m.finish = c.AnthropicDelta.StopReason
	}
	if c.ID != "" && m.id == "" {
		m.id = c.ID
	}
	if c.Message != nil && c.Message.ID != "" && m.id == "" {
		m.id = c.Message.ID
	}
	if c.Usage != nil {
		m.mergeUsage(c.Usage)
	}
	if c.Message != nil && c.Message.Usage != nil {
		m.mergeUsage(c.Message.Usage)
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			m.finish = *ch.FinishReason
		}
		part := ch.Delta
		if part == nil {
			part = ch.Message
		}
		if part == nil {
			continue
		}
		for i, tc := range part.ToolCalls {
			idx := m.toolKey(i, tc.Index, tc.ID, ch.Message != nil)
			t, ok := m.tools[idx]
			if !ok {
				t = &ToolCall{}
				m.tools[idx] = t
				m.order = append(m.order, idx)
			}
			m.last = idx
			if tc.ID != "" && len(tc.ID) <= 256 {
				if _, known := m.byID[tc.ID]; !known {
					m.byID[tc.ID] = idx
				}
			}
			if tc.ID != "" && t.ID == "" && len(tc.ID) <= 256 {
				t.ID = tc.ID
			}
			if tc.Function.Name != "" {
				t.Name = tc.Function.Name
			}
			if len(t.Arguments) < 8<<10 {
				t.Arguments += tc.Function.Arguments
			}
		}
	}
}

// toolKey ordnet ein Stück einem Aufruf zu: nach index, wenn vorhanden; sonst nach id (ein
// Stück mit bekannter id setzt diesen Aufruf fort, eine neue id beginnt einen); ohne beides
// gehört es zum zuletzt begonnenen Aufruf. In einer ganzen Nachricht (JSON) zählt die Stelle.
func (m *meter) toolKey(pos int, index *int, id string, whole bool) int {
	if index != nil {
		return *index
	}
	if whole {
		return pos
	}
	if id != "" {
		if k, ok := m.byID[id]; ok {
			return k
		}
		k := m.nextKey
		m.nextKey--
		return k
	}
	if _, ok := m.tools[m.last]; ok {
		return m.last
	}
	k := m.nextKey
	m.nextKey--
	return k
}

type msgPart struct {
	ToolCalls []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// mergeUsage übernimmt Werte ungleich null (Anthropic meldet Eingabe und
// Ausgabe in verschiedenen Ereignissen).
func (m *meter) mergeUsage(u *rawUsage) {
	if m.usage == nil {
		m.usage = &rawUsage{}
	}
	d := m.usage
	for _, p := range []struct{ dst, src *int64 }{
		{&d.PromptTokens, &u.PromptTokens}, {&d.CompletionTokens, &u.CompletionTokens},
		{&d.PromptCacheHitTokens, &u.PromptCacheHitTokens}, {&d.InputTokens, &u.InputTokens},
		{&d.OutputTokens, &u.OutputTokens}, {&d.CacheReadInputTokens, &u.CacheReadInputTokens},
		{&d.CacheCreationInputTokens, &u.CacheCreationInputTokens},
	} {
		if *p.src != 0 {
			*p.dst = *p.src
		}
	}
	if u.PromptTokensDetails != nil {
		d.PromptTokensDetails = u.PromptTokensDetails
	}
}

// Result liefert das Gesammelte. Tokens wie bei pi: Eingabe ohne Cache-Treffer.
func (m *meter) Result() Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sse && m.buf.Len() > 0 {
		m.chunk(m.buf.Bytes())
		m.buf.Reset()
	}
	var c Call
	c.ResponseID = m.id
	if u := m.usage; u != nil {
		if u.InputTokens != 0 || u.OutputTokens != 0 { // Anthropic
			c.Usage = config.Usage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
		} else {
			cached := u.PromptCacheHitTokens
			if cached == 0 && u.PromptTokensDetails != nil {
				cached = u.PromptTokensDetails.CachedTokens
			}
			c.Usage = config.Usage{Input: u.PromptTokens - cached, Output: u.CompletionTokens, CacheRead: cached}
		}
	}
	c.FinishReason = strings.ToValidUTF8(m.finish, "")
	c.Complete = m.finish != "" || m.stopped || m.jsonOK
	// Aufrufe mit index nach index, solche ohne in der Reihenfolge ihres Auftretens.
	sort.SliceStable(m.order, func(i, j int) bool {
		a, b := m.order[i], m.order[j]
		if a >= 0 && b >= 0 {
			return a < b
		}
		return a >= 0 && b < 0
	})
	for _, i := range m.order {
		t := *m.tools[i]
		t.Arguments = strings.ToValidUTF8(t.Arguments, "")
		t.ID = strings.ToValidUTF8(t.ID, "")
		c.ToolCalls = append(c.ToolCalls, t)
	}
	return c
}

// meteredBody reicht die Antwort durch und meldet am Ende das Ergebnis.
type meteredBody struct {
	io.ReadCloser
	m    *meter
	once sync.Once
	done func(Call)
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.m.Write(p[:n])
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *meteredBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (b *meteredBody) finish() { b.once.Do(func() { b.done(b.m.Result()) }) }

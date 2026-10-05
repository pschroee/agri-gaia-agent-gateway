// Package fakellm ist ein geskriptetes, OpenAI-kompatibles Modell für
// Integrationstests (E9). Es antwortet im SSE-Format von chat/completions.
//
// Skript: Die erste Nutzernachricht, die Zeilen der Form "CALL <werkzeug> <json>"
// enthält, legt die Werkzeugaufrufe fest. Die n-te Antwort des Modells nach
// dieser Nachricht ruft den n-ten auf; danach antwortet es mit "fertig". So
// steuert ein Test auch Subagenten: Deren Auftrag enthält eigene CALL-Zeilen.
//
// Platzhalter in den Argumenten: {{fullOutputPath}} wird durch den letzten Pfad der Form
// „Full output: /tmp/pi-bash-….log“ aus den bisherigen Werkzeugergebnissen ersetzt (H1).
package fakellm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

var fullOutputRe = regexp.MustCompile(`Full output: (/tmp/pi-bash-[0-9a-f]+\.log)`)

// Weitere Platzhalter: {{replyTo}} ist die Kennung der letzten Anfrage eines Subagenten
// (contact_supervisor, „replyTo: "…"“), {{runId}} die Kennung des letzten Laufs im Hintergrund
// („Async: <agent> [<id>]“).
var (
	replyToRe = regexp.MustCompile(`replyTo: "([0-9a-f-]{8,})"`)
	runIDRe   = regexp.MustCompile(`Async: [a-z-]+ \[([0-9a-f-]{8,})\]`)
	// {{peer}}: anderer Subagent in der Liste von intercom („(subagent-worker-1a2b3c4d)“).
	peerRe = regexp.MustCompile(`\((subagent-[a-z]+-[0-9a-f]{6,})\)`)
)

// Issued ist ein vom Modell angeforderter Werkzeugaufruf.
type Issued struct {
	ID   string
	Tool string
	Args string
}

type Server struct {
	n      atomic.Int64
	mu     sync.Mutex
	issued []Issued
	seen   []string // Texte aller Nachrichten, die das Modell zu sehen bekam
}

// Seen liefert die Texte aller Nachrichten der bisherigen Anfragen (Haupt- und Kind-Sitzungen).
func (s *Server) Seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *Server) Issued() []Issued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Issued(nil), s.issued...)
}

func text(c json.RawMessage) string {
	var str string
	if json.Unmarshal(c, &str) == nil {
		return str
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(c, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return b.String()
}

type call struct{ tool, args string }

// Script liefert die CALL-Zeilen eines Textes.
func script(t string) []call {
	var cs []call
	for _, line := range strings.Split(t, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "CALL "); ok {
			tool, args, _ := strings.Cut(rest, " ")
			if args == "" {
				args = "{}"
			}
			cs = append(cs, call{tool, args})
		}
	}
	return cs
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	for _, m := range req.Messages {
		s.seen = append(s.seen, text(m.Content))
	}
	s.mu.Unlock()
	var sc []call
	start := -1
	for i, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		if cs := script(text(m.Content)); len(cs) > 0 {
			sc, start = cs, i
			break
		}
	}
	k := 0
	if start >= 0 {
		for _, m := range req.Messages[start+1:] {
			if m.Role == "assistant" {
				k++
			}
		}
	}
	id := s.n.Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	rid := fmt.Sprintf("fake-%d", id)
	usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	chunk := func(delta map[string]any, finish any) map[string]any {
		c := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			c["finish_reason"] = finish
		}
		m := map[string]any{"id": rid, "object": "chat.completion.chunk", "choices": []any{c}}
		if finish != nil {
			m["usage"] = usage
		}
		return m
	}
	if k < len(sc) {
		c := sc[k]
		if strings.Contains(c.args, "{{fullOutputPath}}") {
			last := ""
			for _, m := range req.Messages {
				if ms := fullOutputRe.FindAllStringSubmatch(text(m.Content), -1); len(ms) > 0 {
					last = ms[len(ms)-1][1]
				}
			}
			c.args = strings.ReplaceAll(c.args, "{{fullOutputPath}}", last)
		}
		// Platzhalter in einem Auftrag an einen Subagenten (Feld task) ersetzt erst der Subagent selbst.
		nested := strings.Contains(c.args, `"task"`)
		for ph, re := range map[string]*regexp.Regexp{"{{replyTo}}": replyToRe, "{{runId}}": runIDRe} {
			if nested {
				break
			}
			if !strings.Contains(c.args, ph) {
				continue
			}
			last := ""
			for _, m := range req.Messages {
				if ms := re.FindAllStringSubmatch(text(m.Content), -1); len(ms) > 0 {
					last = ms[len(ms)-1][1]
				}
			}
			c.args = strings.ReplaceAll(c.args, ph, last)
		}
		if !nested && strings.Contains(c.args, "{{peer}}") {
			// Name eines anderen Subagenten aus der letzten Liste von intercom (nicht die eigene Sitzung).
			last := ""
			for _, m := range req.Messages {
				for _, line := range strings.Split(text(m.Content), "\n") {
					if mm := peerRe.FindStringSubmatch(line); mm != nil && !strings.Contains(line, "[self") {
						last = mm[1]
					}
				}
			}
			c.args = strings.ReplaceAll(c.args, "{{peer}}", last)
		}
		cid := fmt.Sprintf("call_fake_%d", id)
		s.mu.Lock()
		s.issued = append(s.issued, Issued{ID: cid, Tool: c.tool, Args: c.args})
		s.mu.Unlock()
		send(chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": cid, "type": "function",
			"function": map[string]any{"name": c.tool, "arguments": c.args}}}}, nil))
		send(chunk(map[string]any{}, "tool_calls"))
	} else {
		send(chunk(map[string]any{"role": "assistant", "content": "fertig"}, nil))
		send(chunk(map[string]any{}, "stop"))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// Package fakellm is a scripted, OpenAI-compatible model for
// integration tests (E9). It answers in the SSE format of chat/completions.
//
// Script: the first user message containing lines of the form "CALL <tool> <json>"
// determines the tool calls. The model's n-th response after
// this message calls the n-th one; after that it answers with "done". This way
// a test also controls subagents: their task contains its own CALL lines.
//
// Placeholders in the arguments: {{fullOutputPath}} is replaced by the last path of the form
// "Full output: /tmp/pi-bash-….log" from the tool results so far (H1).
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

// Further placeholders: {{replyTo}} is the ID of a subagent's last request
// (contact_supervisor, `replyTo: "…"`), {{runId}} the ID of the last background run
// ("Async: <agent> [<id>]").
var (
	replyToRe = regexp.MustCompile(`replyTo: "([0-9a-f-]{8,})"`)
	runIDRe   = regexp.MustCompile(`Async: [a-z-]+ \[([0-9a-f-]{8,})\]`)
	// {{peer}}: another subagent in intercom's list ("(subagent-worker-1a2b3c4d)").
	peerRe = regexp.MustCompile(`\((subagent-[a-z]+-[0-9a-f]{6,})\)`)
)

// Issued is a tool call requested by the model.
type Issued struct {
	ID   string
	Tool string
	Args string
}

type Server struct {
	n      atomic.Int64
	mu     sync.Mutex
	issued []Issued
	seen   []string // texts of all messages the model got to see
}

// Seen returns the texts of all messages of the requests so far (main and child sessions).
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

// script returns the CALL lines of a text.
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
		// Placeholders in a task for a subagent (field task) are replaced only by the subagent itself.
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
			// Name of another subagent from intercom's last list (not the own session).
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
		send(chunk(map[string]any{"role": "assistant", "content": "done"}, nil))
		send(chunk(map[string]any{}, "stop"))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

package chat

// Subagents: billing at the proxy, visibility from the subagents' session
// files and the limit on how many a chat may start.
//
// Two levels of the limit:
//   - hard, outside the sandbox: the LLM proxy allows at most
//     1 + max_subagents concurrent model calls per chat, and the orchestrator
//     aborts as soon as more subagents have been started than allowed;
//   - cooperative: pi-subagents gets the same limit in its configuration, so
//     that the agent knows it and is refused cleanly.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agw/internal/llmproxy"
	"agw/internal/sock"
	"agw/internal/store"
)

// --- Billing at the proxy (llmproxy.Recorder) ---

// Attribute assigns a source address in the slot network to a chat.
func (m *Manager) Attribute(ip string) llmproxy.Attribution {
	if ip == "" {
		return llmproxy.Attribution{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for chatID, l := range m.live {
		if l.ip == ip {
			return llmproxy.Attribution{ChatID: chatID, SlotID: l.slot.ID, MaxConcurrent: 1 + l.maxSub}
		}
	}
	return llmproxy.Attribution{}
}

// Record stores a model call recorded at the proxy.
func (m *Manager) Record(c llmproxy.Call) {
	tc, _ := json.Marshal(c.ToolCalls)
	if len(c.ToolCalls) == 0 {
		tc = []byte("[]")
	}
	ctx := context.Background()
	rec, err := m.st.AddLLMCall(ctx, store.LLMCall{
		ChatID: c.ChatID, SlotID: c.SlotID, SourceIP: c.SourceIP, Model: c.Model, ResponseID: c.ResponseID,
		Status: c.Status, Input: c.Usage.Input, Output: c.Usage.Output, CacheRead: c.Usage.CacheRead, CacheWrite: c.Usage.CacheWrite,
		Cost: c.Cost, Peak: c.Peak, ToolCalls: tc, StartedAt: c.StartedAt, DurationMs: c.DurationMs,
		FinishReason: c.FinishReason, Complete: c.Complete,
	})
	if err != nil {
		slog.Error("model call not stored", "chat", c.ChatID, "err", err)
		return
	}
	m.publish(c.ChatID, Event{Kind: "llm_call", Data: rec})
	m.publishChat(ctx, c.ChatID)
}

// LimitHit: the proxy refused a call because of the limit. The detail and the error message are
// matched by the CLI and the web UI and stay German until they are changed on all sides.
func (m *Manager) LimitHit(chatID string, max int) {
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	slot := ""
	if l != nil {
		slot = l.slot.ID
	}
	m.LogCall(slot, chatID, "proxy", "agent_limit", fmt.Sprintf("at most %d concurrent agents", max), "refused")
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": fmt.Sprintf("model call refused: at most %d concurrent agents (main agent and %d subagents) allowed", max, max-1)}})
}

// --- Limit per chat ---

// SetMaxSubagents changes the limit; with an active chat it takes effect immediately.
func (m *Manager) SetMaxSubagents(ctx context.Context, chatID string, n int) (ChatView, error) {
	if n < 0 || n > m.opt.MaxSubagentsLimit {
		return ChatView{}, fmt.Errorf("%w: max_subagents must be between 0 and %d", ErrInvalid, m.opt.MaxSubagentsLimit)
	}
	unlock := m.lock(chatID)
	defer unlock()
	_, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	if err := m.st.SetMaxSubagents(ctx, chatID, n); err != nil {
		return ChatView{}, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	if l != nil {
		l.maxSub = n
	}
	m.mu.Unlock()
	if l != nil {
		m.applySubagentConfig(l, n)
	}
	slog.Info("subagent limit set", "chat", chatID, "max", n)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// applySubagentConfig writes the limit into the configuration of
// pi-subagents (cooperative level). The sandbox can change the file; the limit
// is therefore only hard through the proxy and monitoring.
func (m *Manager) applySubagentConfig(l *live, n int) {
	cfg := map[string]any{"maxActiveAsyncRunsPerSession": max(n, 1), "globalConcurrencyLimit": max(n, 1)}
	if n > 0 {
		cfg["maxSubagentSpawnsPerSession"] = n
	} else {
		cfg["maxSubagentSpawnsPerSession"] = 1 // 0 would mean "unlimited" there; 0 is enforced hard via proxy and monitoring
	}
	b, _ := json.Marshal(cfg)
	_, err := execPiT(l.slot.Worker, []string{"agw-exec", "put", "/agent/config/extensions/subagent/config.json"}, bytes.NewReader(b), callTimeout)
	if err != nil {
		slog.Warn("pi-subagents configuration not written", "err", err)
	}
}

// --- Visibility: the subagents' session files ---

// runKey turns the path into the run's ID (with #n for parallel children);
// the same ID is attached to the tool executions (E9).
func runKey(path string) string {
	if k := sock.SessionKey(path); k != "main" {
		return k
	}
	return ""
}

const maxEntryText = 4000

var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func clip(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > maxEntryText {
		return s[:maxEntryText] + " … [truncated]"
	}
	return s
}

// parseChildSession turns new lines of a subagent session into entries.
func parseChildSession(chatID, run, agent, data string) []store.SubagentEntry {
	var out []store.SubagentEntry
	add := func(id, kind, resp string, payload any) {
		b, _ := json.Marshal(payload)
		out = append(out, store.SubagentEntry{ChatID: chatID, RunID: run, EntryID: id, Agent: agent, Kind: kind, Payload: b, ResponseID: resp})
	}
	for _, line := range strings.Split(data, "\n") {
		var e struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Name    string `json:"name"`
			Message struct {
				Role       string `json:"role"`
				ResponseID string `json:"responseId"`
				ToolName   string `json:"toolName"`
				ToolCallID string `json:"toolCallId"`
				IsError    bool   `json:"isError"`
				Content    []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Text      string          `json:"text"`
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.ID == "" {
			continue
		}
		// pi-subagents names the child session "<agent>: <task …>"; fallback when no status file
		// names the agent (e.g. for runs in the foreground).
		if e.Type == "session_info" && agent == "" {
			if a, _, ok := strings.Cut(e.Name, ":"); ok && agentNameRe.MatchString(a) {
				agent = a
			}
			continue
		}
		if e.Type != "message" {
			continue
		}
		msg := e.Message
		switch msg.Role {
		case "user":
			var b strings.Builder
			for _, c := range msg.Content {
				b.WriteString(c.Text)
			}
			add(e.ID, "task", "", map[string]string{"text": clip(b.String())})
		case "assistant":
			for i, c := range msg.Content {
				id := e.ID + ":" + strconv.Itoa(i)
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						add(id, "text", msg.ResponseID, map[string]string{"text": clip(c.Text)})
					}
				case "toolCall":
					add(id, "tool_call", msg.ResponseID, map[string]string{"name": c.Name, "arguments": clip(string(c.Arguments)), "id": c.ID})
				}
			}
		case "toolResult":
			var b strings.Builder
			for _, c := range msg.Content {
				b.WriteString(c.Text)
			}
			add(e.ID, "tool_result", "", map[string]any{"name": msg.ToolName, "text": clip(b.String()), "is_error": msg.IsError, "tool_call_id": msg.ToolCallID})
		}
	}
	return out
}

// watchSubagents keeps reading the subagents' session files while the chat
// is active and enforces the limit.
func (m *Manager) watchSubagents(chatID string, l *live) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	offsets := map[string]int64{}
	runs := map[string]runInfo{}
	agents := map[string]string{} // agent per run, once recognised (session_info is only at the start of the file)
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		m.pollSubagents(chatID, l, offsets, runs, agents)
	}
}

// runInfo: name and state of a run, as agw-exec poll-subagents reads them from pi-subagents'
// status files.
type runInfo struct {
	Agent     string `json:"agent"`
	Label     string `json:"label"`
	State     string `json:"state"`
	PiRun     string `json:"pi_run"`
	Parent    string `json:"parent"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"`
}

func msTime(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms)
	return &t
}

func (m *Manager) pollSubagents(chatID string, l *live, offsets map[string]int64, runs map[string]runInfo, agents map[string]string) {
	in, _ := json.Marshal(map[string]any{"offsets": offsets})
	// The session files live in pi's container; since E9 the agent can no
	// longer reach them. They are read with agw-exec (there is no Python there).
	out, err := execPiT(l.slot.Worker, []string{"agw-exec", "poll-subagents"}, bytes.NewReader(in), 10*time.Second)
	if err != nil || len(out) == 0 || len(out) > 4<<20 {
		return
	}
	var res struct {
		Files []struct {
			Path   string `json:"path"`
			Offset int64  `json:"offset"`
			Data   string `json:"data"`
		} `json:"files"`
		Agents map[string]string  `json:"agents"`
		Runs   map[string]runInfo `json:"runs"`
	}
	if json.Unmarshal(out, &res) != nil {
		return
	}
	ctx := context.Background()
	changed := false
	for key, ri := range res.Runs {
		if len(key) > 80 || runs[key] == ri {
			continue
		}
		r, err := m.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: chatID, RunID: key, Agent: short(ri.Agent), Label: short(ri.Label),
			State: short(ri.State), PiRunID: short(ri.PiRun), ParentRunID: short(ri.Parent), StartedAt: msTime(ri.StartedAt), EndedAt: msTime(ri.EndedAt)})
		if err != nil {
			slog.Warn("subagent run not stored", "chat", chatID, "err", err)
			continue
		}
		runs[key] = ri
		changed = true
		m.publish(chatID, Event{Kind: "subagent_run", Data: r})
	}
	var fresh []store.SubagentEntry
	for _, f := range res.Files {
		run := runKey(f.Path)
		if run == "" || f.Offset <= offsets[f.Path] {
			continue
		}
		offsets[f.Path] = f.Offset
		base, _, _ := strings.Cut(run, "#")
		agent := res.Agents[base]
		if agent == "" {
			agent = res.Runs[run].Agent
		}
		if agent == "" {
			agent = agents[run]
		}
		parsed := parseChildSession(chatID, run, short(agent), f.Data)
		if len(parsed) > 0 && parsed[0].Agent != "" {
			agents[run] = parsed[0].Agent
		}
		added, err := m.st.AddSubagentEntries(ctx, parsed)
		if err != nil {
			slog.Warn("subagent entries not stored", "chat", chatID, "err", err)
		}
		fresh = append(fresh, added...)
	}
	if len(fresh) == 0 {
		if changed {
			m.publishChat(ctx, chatID)
		}
		return
	}
	for _, e := range fresh {
		m.publish(chatID, Event{Kind: "subagent", Data: e})
	}
	m.enforceSubagentLimit(ctx, chatID, l)
	m.publishChat(ctx, chatID)
}

// enforceSubagentLimit aborts when more subagents have been started than
// allowed: abort the main agent (ends foreground children) and end all other
// node processes of the sandbox except pi itself (PID 1). Detail, result and
// error message are matched by the CLI and the web UI and stay German for now.
func (m *Manager) enforceSubagentLimit(ctx context.Context, chatID string, l *live) {
	n, err := m.st.SubagentRunCount(ctx, chatID)
	if err != nil {
		return
	}
	m.mu.Lock()
	limit := l.maxSub
	already := l.limitEnforcedAt >= n
	if n > limit && !already {
		l.limitEnforcedAt = n
	}
	m.mu.Unlock()
	if n <= limit || already {
		return
	}
	slog.Warn("subagent limit exceeded, aborting", "chat", chatID, "started", n, "allowed", limit)
	_, _ = callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second)
	_, _ = execPiT(l.slot.Worker, []string{"agw-exec", "kill-node"}, nil, 10*time.Second)
	m.LogCall(l.slot.ID, chatID, "orchestrator", "subagent_limit", fmt.Sprintf("%d started, %d allowed", n, limit), "aborted")
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": fmt.Sprintf("limit exceeded: %d subagents started, %d allowed. The turn was aborted.", n, limit)}})
}

// SubagentEntries for the chat view.
func (m *Manager) SubagentEntries(ctx context.Context, chatID string) ([]store.SubagentEntry, error) {
	return m.st.ListSubagentEntries(ctx, chatID)
}

// LLMCalls for the chat view.
func (m *Manager) LLMCalls(ctx context.Context, chatID string) ([]store.LLMCall, error) {
	return m.st.ListLLMCalls(ctx, chatID)
}

// short shortens data from the sandbox for storage and display.
func short(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// SubagentRuns for the chat view.
func (m *Manager) SubagentRuns(ctx context.Context, chatID string) ([]store.SubagentRun, error) {
	return m.st.ListSubagentRuns(ctx, chatID)
}

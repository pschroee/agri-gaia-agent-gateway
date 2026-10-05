package chat

// Subagenten: Abrechnung am Proxy, Sichtbarkeit aus den Sitzungsdateien der
// Subagenten und die Grenze, wie viele ein Chat starten darf.
//
// Zwei Ebenen der Grenze:
//   - hart, außerhalb der Sandbox: der LLM-Proxy lässt je Chat höchstens
//     1 + max_subagents Modellaufrufe gleichzeitig zu, und der Orchestrator
//     bricht ab, sobald mehr Subagenten gestartet wurden als erlaubt;
//   - kooperativ: pi-subagents bekommt dieselbe Grenze in seiner
//     Konfiguration, damit der Agent sie kennt und sauber abgewiesen wird.

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

// --- Abrechnung am Proxy (llmproxy.Recorder) ---

// Attribute ordnet eine Quelladresse im Platz-Netz einem Chat zu.
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

// Record speichert einen am Proxy erfassten Modellaufruf.
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
		slog.Error("Modellaufruf nicht gespeichert", "chat", c.ChatID, "fehler", err)
		return
	}
	m.publish(c.ChatID, Event{Kind: "llm_call", Data: rec})
	m.publishChat(ctx, c.ChatID)
}

// LimitHit: der Proxy hat einen Aufruf wegen der Grenze abgewiesen.
func (m *Manager) LimitHit(chatID string, max int) {
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	slot := ""
	if l != nil {
		slot = l.slot.ID
	}
	m.LogCall(slot, chatID, "proxy", "agent_limit", fmt.Sprintf("höchstens %d gleichzeitige Agenten", max), "abgewiesen")
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": fmt.Sprintf("Modellaufruf abgewiesen: höchstens %d gleichzeitige Agenten (Hauptagent und %d Subagenten) erlaubt", max, max-1)}})
}

// --- Grenze je Chat ---

// SetMaxSubagents ändert die Grenze; bei aktivem Chat wirkt sie sofort.
func (m *Manager) SetMaxSubagents(ctx context.Context, chatID string, n int) (ChatView, error) {
	if n < 0 || n > m.opt.MaxSubagentsLimit {
		return ChatView{}, fmt.Errorf("%w: max_subagents muss zwischen 0 und %d liegen", ErrInvalid, m.opt.MaxSubagentsLimit)
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
	slog.Info("Grenze für Subagenten gesetzt", "chat", chatID, "max", n)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// applySubagentConfig schreibt die Grenze in die Konfiguration von
// pi-subagents (kooperative Ebene). Die Sandbox kann die Datei ändern; hart
// ist die Grenze deshalb erst durch Proxy und Überwachung.
func (m *Manager) applySubagentConfig(l *live, n int) {
	cfg := map[string]any{"maxActiveAsyncRunsPerSession": max(n, 1), "globalConcurrencyLimit": max(n, 1)}
	if n > 0 {
		cfg["maxSubagentSpawnsPerSession"] = n
	} else {
		cfg["maxSubagentSpawnsPerSession"] = 1 // 0 hieße dort „unbegrenzt“; hart gilt 0 über Proxy und Überwachung
	}
	b, _ := json.Marshal(cfg)
	_, err := execPiT(l.slot.Worker, []string{"agw-exec", "put", "/agent/config/extensions/subagent/config.json"}, bytes.NewReader(b), callTimeout)
	if err != nil {
		slog.Warn("pi-subagents-Konfiguration nicht geschrieben", "fehler", err)
	}
}

// --- Sichtbarkeit: Sitzungsdateien der Subagenten ---

// runKey macht aus dem Pfad die Kennung des Laufs (bei parallelen Kindern
// mit #n); dieselbe Kennung steht an den Werkzeugausführungen (E9).
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
		return s[:maxEntryText] + " … [gekürzt]"
	}
	return s
}

// parseChildSession wandelt neue Zeilen einer Subagenten-Sitzung in Einträge.
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
		// pi-subagents benennt die Kind-Sitzung „<agent>: <Auftrag …>“; Rückfall, wenn keine
		// Statusdatei den Agenten nennt (etwa bei Läufen im Vordergrund).
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

// watchSubagents liest die Sitzungsdateien der Subagenten laufend nach,
// solange der Chat aktiv ist, und setzt die Grenze durch.
func (m *Manager) watchSubagents(chatID string, l *live) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	offsets := map[string]int64{}
	runs := map[string]runInfo{}
	agents := map[string]string{} // Agent je Lauf, einmal erkannt (session_info steht nur am Anfang der Datei)
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		m.pollSubagents(chatID, l, offsets, runs, agents)
	}
}

// runInfo: Name und Zustand eines Laufs, wie agw-exec poll-subagents sie aus den Statusdateien
// von pi-subagents liest.
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
	// Die Sitzungsdateien liegen im Container von pi; der Agent erreicht sie
	// seit E9 nicht mehr. Gelesen wird mit agw-exec (dort gibt es kein Python).
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
			slog.Warn("Subagenten-Lauf nicht gespeichert", "chat", chatID, "fehler", err)
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
			slog.Warn("Subagenten-Einträge nicht gespeichert", "chat", chatID, "fehler", err)
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

// enforceSubagentLimit bricht ab, wenn mehr Subagenten gestartet wurden als
// erlaubt: Hauptagent abbrechen (beendet Vordergrund-Kinder) und alle übrigen
// node-Prozesse der Sandbox außer pi selbst (PID 1) beenden.
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
	slog.Warn("Grenze für Subagenten überschritten, breche ab", "chat", chatID, "gestartet", n, "erlaubt", limit)
	_, _ = callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second)
	_, _ = execPiT(l.slot.Worker, []string{"agw-exec", "kill-node"}, nil, 10*time.Second)
	m.LogCall(l.slot.ID, chatID, "orchestrator", "subagent_limit", fmt.Sprintf("%d gestartet, %d erlaubt", n, limit), "abgebrochen")
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": fmt.Sprintf("Grenze überschritten: %d Subagenten gestartet, erlaubt sind %d. Der Durchgang wurde abgebrochen.", n, limit)}})
}

// SubagentEntries für die Chat-Ansicht.
func (m *Manager) SubagentEntries(ctx context.Context, chatID string) ([]store.SubagentEntry, error) {
	return m.st.ListSubagentEntries(ctx, chatID)
}

// LLMCalls für die Chat-Ansicht.
func (m *Manager) LLMCalls(ctx context.Context, chatID string) ([]store.LLMCall, error) {
	return m.st.ListLLMCalls(ctx, chatID)
}

// short kürzt Angaben aus der Sandbox für Speicher und Anzeige.
func short(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// SubagentRuns für die Chat-Ansicht.
func (m *Manager) SubagentRuns(ctx context.Context, chatID string) ([]store.SubagentRun, error) {
	return m.st.ListSubagentRuns(ctx, chatID)
}

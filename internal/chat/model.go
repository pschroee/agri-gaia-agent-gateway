package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"agw/internal/store"
)

// ErrContextTooLarge: the chat's context does not fit into the new model (ContextTooLargeError).
var ErrContextTooLarge = errors.New("context too large for the model")

// ContextTooLargeError states why a model switch is blocked. Limit is the threshold from which pi
// would have to compact in the new model (context window minus reserve).
type ContextTooLargeError struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
	Window int64  `json:"window"`
	Limit  int64  `json:"limit"`
}

func (e *ContextTooLargeError) Error() string {
	return fmt.Sprintf("The context (%d tokens) does not fit into %s (context window %d tokens, usable %d). Compact first, then switch.",
		e.Tokens, e.Model, e.Window, e.Limit)
}

func (e *ContextTooLargeError) Unwrap() error { return ErrContextTooLarge }

// ThinkingLevels are the levels pi knows; which ones a model offers, pi reports per model.
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

var thinkingLabels = map[string]string{
	"off": "off", "minimal": "minimal", "low": "low", "medium": "medium", "high": "high", "xhigh": "very high", "max": "maximum",
}

// checkFits checks whether the most recently measured context fits into the model. Without a
// measurement or without a known context window it counts as fitting.
func (m *Manager) checkFits(c store.Chat, model string) error {
	if len(c.Context) == 0 {
		return nil
	}
	var u ContextUsage
	if json.Unmarshal(c.Context, &u) != nil || u.Tokens == nil {
		return nil
	}
	window := m.cat.ContextWindow(model)
	if window <= 0 {
		return nil
	}
	limit := window - int64(m.opt.CompactReserveTokens)
	if limit <= 0 {
		limit = window
	}
	if *u.Tokens > limit {
		return &ContextTooLargeError{Model: model, Tokens: *u.Tokens, Window: window, Limit: limit}
	}
	return nil
}

// SetModel switches the chat's model (/model). If the context does not fit, it returns
// ContextTooLargeError; with compactFirst it compacts instead and switches afterwards.
// An idle chat gets the model when it is resumed.
func (m *Manager) SetModel(ctx context.Context, chatID, model string, compactFirst bool) (ChatView, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ChatView{}, fmt.Errorf("%w: /model expects a model (provider/model)", ErrInvalid)
	}
	if _, _, ok := m.cat.Lookup(model); !ok {
		return ChatView{}, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	busy := l != nil && (l.running || l.compacting)
	m.mu.Unlock()
	if busy {
		return ChatView{}, ErrRunning
	}
	if c.Model == model {
		m.setPendingModel(chatID, "")
		return m.View(ctx, chatID)
	}
	if err := m.checkFits(c, model); err != nil {
		if !compactFirst {
			return ChatView{}, err
		}
		m.setPendingModel(chatID, model)
		if _, err := m.RunCommand(ctx, chatID, "/compact"); err != nil {
			m.setPendingModel(chatID, "")
			return ChatView{}, err
		}
		m.publishChat(ctx, chatID)
		return m.View(ctx, chatID)
	}
	if err := m.applyModel(ctx, chatID, model); err != nil {
		return ChatView{}, err
	}
	return m.View(ctx, chatID)
}

func (m *Manager) setPendingModel(chatID, model string) {
	m.mu.Lock()
	if model == "" {
		delete(m.pendingModel, chatID)
	} else {
		m.pendingModel[chatID] = model
	}
	m.mu.Unlock()
}

// applyModel sets the model in pi (if active) and in the database. Under the chat lock it checks
// again that pi is not working and the context fits: between the check in SetModel and here a
// request may have started (code review 2026-09-30).
func (m *Manager) applyModel(ctx context.Context, chatID, model string) error {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	l := m.live[chatID]
	busy := l != nil && (l.running || l.compacting || m.sending[chatID])
	m.mu.Unlock()
	if busy {
		return ErrRunning
	}
	if err := m.checkFits(c, model); err != nil {
		return err
	}
	if l != nil {
		prov, mod, _ := m.cat.Lookup(model)
		if _, err := callT(l.slot.Worker, map[string]any{"type": "set_model", "provider": prov.ID, "modelId": mod.ID}, callTimeout); err != nil {
			return fmt.Errorf("set model: %w", err)
		}
	}
	if err := m.st.SetModel(ctx, chatID, model); err != nil {
		return err
	}
	slog.Info("model switched", "chat", chatID, "from", c.Model, "to", model)
	if l != nil {
		// pi adapts the thinking level to the new model; set the desired level again and read it back.
		m.syncThinking(ctx, chatID, model, l.slot.Worker, c.ThinkingLevel)
		m.refreshInfo(ctx, chatID, l.slot.Worker)
	}
	m.publishChat(ctx, chatID)
	return nil
}

// applyPendingModel performs the scheduled model switch after a compaction.
func (m *Manager) applyPendingModel(ctx context.Context, chatID string) {
	m.mu.Lock()
	model := m.pendingModel[chatID]
	delete(m.pendingModel, chatID)
	m.mu.Unlock()
	if model == "" {
		return
	}
	err := m.applyModel(ctx, chatID, model)
	if err != nil {
		slog.Warn("model switch after the compaction failed", "chat", chatID, "model", model, "error", err)
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Model switch after the compaction failed: " + err.Error()}})
	}
	m.publishChat(ctx, chatID)
}

// SetThinkingLevel sets the thinking level (/effort). For an active chat immediately in pi, otherwise
// on resume. It refuses levels the model does not know, provided pi has reported them.
func (m *Manager) SetThinkingLevel(ctx context.Context, chatID, level string) (ChatView, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if !slices.Contains(ThinkingLevels, level) {
		return ChatView{}, fmt.Errorf("%w: /effort expects a level (%s)", ErrInvalid, strings.Join(ThinkingLevels, ", "))
	}
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	busy := l != nil && (l.running || l.compacting)
	known := m.levels[c.Model]
	m.mu.Unlock()
	if busy {
		return ChatView{}, ErrRunning
	}
	if len(known) > 0 && !slices.Contains(known, level) {
		return ChatView{}, fmt.Errorf("%w: %s knows the levels %s", ErrInvalid, c.Model, strings.Join(known, ", "))
	}
	if l != nil {
		if _, err := callT(l.slot.Worker, map[string]any{"type": "set_thinking_level", "level": level}, callTimeout); err != nil {
			return ChatView{}, fmt.Errorf("set thinking level: %w", err)
		}
		m.syncThinking(ctx, chatID, c.Model, l.slot.Worker, "")
	} else if err := m.st.SetThinkingLevel(ctx, chatID, level); err != nil {
		return ChatView{}, err
	}
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// syncThinking sets the desired thinking level (if want is not empty) and then takes over what pi
// actually set, together with the levels the model knows.
func (m *Manager) syncThinking(ctx context.Context, chatID, model string, a Agent, want string) {
	if want != "" {
		if _, err := callT(a, map[string]any{"type": "set_thinking_level", "level": want}, callTimeout); err != nil {
			slog.Warn("thinking level not set", "chat", chatID, "level", want, "error", err)
		}
	}
	if resp, err := callT(a, map[string]any{"type": "get_available_thinking_levels"}, callTimeout); err == nil {
		var d struct {
			Levels []string `json:"levels"`
		}
		var list []string
		if json.Unmarshal(resp.Data, &d) == nil && len(d.Levels) > 0 {
			list = d.Levels
		} else if json.Unmarshal(resp.Data, &list) != nil {
			list = nil
		}
		if len(list) > 0 {
			m.mu.Lock()
			m.levels[model] = list
			m.mu.Unlock()
		}
	}
	if resp, err := callT(a, map[string]any{"type": "get_state"}, callTimeout); err == nil {
		var d struct {
			ThinkingLevel string `json:"thinkingLevel"`
		}
		if json.Unmarshal(resp.Data, &d) == nil && d.ThinkingLevel != "" {
			if err := m.st.SetThinkingLevel(ctx, chatID, d.ThinkingLevel); err != nil {
				slog.Warn("thinking level not saved", "chat", chatID, "error", err)
			}
		}
	}
}

func (m *Manager) modelOptions(current string) []CommandOption {
	var out []CommandOption
	for _, mi := range m.cat.Models() {
		label := mi.Name
		if mi.ContextWindow > 0 {
			label += fmt.Sprintf(" · %s tokens", compactNumber(mi.ContextWindow))
		}
		out = append(out, CommandOption{Value: mi.ID, Label: label, Current: mi.ID == current})
	}
	return out
}

func (m *Manager) effortOptions(model, current string) []CommandOption {
	m.mu.Lock()
	levels := m.levels[model]
	m.mu.Unlock()
	if len(levels) == 0 {
		levels = ThinkingLevels
	}
	out := make([]CommandOption, 0, len(levels))
	for _, l := range levels {
		out = append(out, CommandOption{Value: l, Label: thinkingLabels[l], Current: l == current})
	}
	return out
}

// compactNumber: 1000000 → "1M", 128000 → "128K".
func compactNumber(n int64) string {
	switch {
	case n >= 1_000_000 && n%100_000 == 0:
		s := fmt.Sprintf("%.1f", float64(n)/1e6)
		s = strings.TrimSuffix(s, ".0")
		return s + "M"
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprint(n)
}

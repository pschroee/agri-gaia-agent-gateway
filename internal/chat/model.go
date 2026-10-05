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

// ErrContextTooLarge: Der Kontext des Chats passt nicht in das neue Modell (ContextTooLargeError).
var ErrContextTooLarge = errors.New("Kontext zu groß für das Modell")

// ContextTooLargeError nennt, warum ein Modellwechsel gesperrt ist. Limit ist die Schwelle, ab der
// pi im neuen Modell kompaktieren müsste (Kontextfenster minus Reserve).
type ContextTooLargeError struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
	Window int64  `json:"window"`
	Limit  int64  `json:"limit"`
}

func (e *ContextTooLargeError) Error() string {
	return fmt.Sprintf("Der Kontext (%d Tokens) passt nicht in %s (Kontextfenster %d Tokens, nutzbar %d). Erst kompaktieren, dann wechseln.",
		e.Tokens, e.Model, e.Window, e.Limit)
}

func (e *ContextTooLargeError) Unwrap() error { return ErrContextTooLarge }

// ThinkingLevels sind die Stufen, die pi kennt; welche ein Modell anbietet, meldet pi je Modell.
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

var thinkingLabels = map[string]string{
	"off": "aus", "minimal": "minimal", "low": "niedrig", "medium": "mittel", "high": "hoch", "xhigh": "sehr hoch", "max": "maximal",
}

// checkFits prüft, ob der zuletzt gemessene Kontext in das Modell passt. Ohne Messung oder ohne
// bekanntes Kontextfenster gilt er als passend.
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

// SetModel wechselt das Modell des Chats (/model). Passt der Kontext nicht, liefert es
// ContextTooLargeError; mit compactFirst kompaktiert es stattdessen und wechselt danach.
// Ein ruhender Chat bekommt das Modell beim Fortsetzen.
func (m *Manager) SetModel(ctx context.Context, chatID, model string, compactFirst bool) (ChatView, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ChatView{}, fmt.Errorf("%w: /model erwartet ein Modell (anbieter/modell)", ErrInvalid)
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

// applyModel setzt das Modell in pi (falls aktiv) und in der Datenbank. Unter der Chat-Sperre prüft es
// erneut, dass pi nicht arbeitet und der Kontext passt: Zwischen der Prüfung in SetModel und hier kann
// ein Auftrag begonnen haben (Code-Review 30.09.2026).
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
			return fmt.Errorf("Modell setzen: %w", err)
		}
	}
	if err := m.st.SetModel(ctx, chatID, model); err != nil {
		return err
	}
	slog.Info("Modell gewechselt", "chat", chatID, "von", c.Model, "zu", model)
	if l != nil {
		// pi passt die Denkstufe an das neue Modell an; die gewünschte Stufe erneut setzen und lesen.
		m.syncThinking(ctx, chatID, model, l.slot.Worker, c.ThinkingLevel)
		m.refreshInfo(ctx, chatID, l.slot.Worker)
	}
	m.publishChat(ctx, chatID)
	return nil
}

// applyPendingModel führt nach einer Kompaktierung den vorgemerkten Modellwechsel aus.
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
		slog.Warn("Modellwechsel nach der Kompaktierung gescheitert", "chat", chatID, "modell", model, "fehler", err)
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Modellwechsel nach der Kompaktierung gescheitert: " + err.Error()}})
	}
	m.publishChat(ctx, chatID)
}

// SetThinkingLevel stellt die Denkstufe ein (/effort). Bei aktivem Chat sofort in pi, sonst beim
// Fortsetzen. Stufen, die das Modell nicht kennt, lehnt es ab, sofern pi sie gemeldet hat.
func (m *Manager) SetThinkingLevel(ctx context.Context, chatID, level string) (ChatView, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if !slices.Contains(ThinkingLevels, level) {
		return ChatView{}, fmt.Errorf("%w: /effort erwartet eine Stufe (%s)", ErrInvalid, strings.Join(ThinkingLevels, ", "))
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
		return ChatView{}, fmt.Errorf("%w: %s kennt die Stufen %s", ErrInvalid, c.Model, strings.Join(known, ", "))
	}
	if l != nil {
		if _, err := callT(l.slot.Worker, map[string]any{"type": "set_thinking_level", "level": level}, callTimeout); err != nil {
			return ChatView{}, fmt.Errorf("Denkstufe setzen: %w", err)
		}
		m.syncThinking(ctx, chatID, c.Model, l.slot.Worker, "")
	} else if err := m.st.SetThinkingLevel(ctx, chatID, level); err != nil {
		return ChatView{}, err
	}
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// syncThinking setzt (wenn want nicht leer) die gewünschte Denkstufe und übernimmt dann, was pi
// tatsächlich eingestellt hat, samt der Stufen, die das Modell kennt.
func (m *Manager) syncThinking(ctx context.Context, chatID, model string, a Agent, want string) {
	if want != "" {
		if _, err := callT(a, map[string]any{"type": "set_thinking_level", "level": want}, callTimeout); err != nil {
			slog.Warn("Denkstufe nicht gesetzt", "chat", chatID, "stufe", want, "fehler", err)
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
				slog.Warn("Denkstufe nicht gespeichert", "chat", chatID, "fehler", err)
			}
		}
	}
}

func (m *Manager) modelOptions(current string) []CommandOption {
	var out []CommandOption
	for _, mi := range m.cat.Models() {
		label := mi.Name
		if mi.ContextWindow > 0 {
			label += fmt.Sprintf(" · %s Tokens", compactNumber(mi.ContextWindow))
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

// compactNumber: 1000000 → „1 Mio.“, 128000 → „128 Tsd.“.
func compactNumber(n int64) string {
	switch {
	case n >= 1_000_000 && n%100_000 == 0:
		s := fmt.Sprintf("%.1f", float64(n)/1e6)
		s = strings.TrimSuffix(strings.Replace(s, ".", ",", 1), ",0")
		return s + " Mio."
	case n >= 1000:
		return fmt.Sprintf("%d Tsd.", n/1000)
	}
	return fmt.Sprint(n)
}

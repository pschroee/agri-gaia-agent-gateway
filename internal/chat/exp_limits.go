// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agw/internal/store"
)

// X3.5: the limit that ended an experiment run. The values go into chats.experiment->>'limit' and into the
// proxy's 429 answer ("experiment run limit reached: <reason>").
const (
	ExpLimitCost     = "cost"
	ExpLimitTokens   = "tokens"
	ExpLimitDuration = "duration"
)

// expGateEntry is what the LLM proxy needs about a chat (X3.1, X3.6), held for planCacheTTL so that the
// proxy does not put a query behind every model call.
type expGateEntry struct {
	model   string
	exp     *store.Experiment
	created time.Time
	at      time.Time
}

// chatGate returns the chat's model, its experiment profile (nil for a product chat) and the time the chat was
// created (the run's duration counts from there). The values are held in memory for planCacheTTL.
func (m *Manager) chatGate(chatID string) (string, *store.Experiment, time.Time) {
	now := time.Now()
	m.mu.Lock()
	if e, ok := m.expCache[chatID]; ok && now.Sub(e.at) < planCacheTTL {
		m.mu.Unlock()
		return e.model, e.exp, e.created
	}
	m.mu.Unlock()
	model, exp, created, err := m.st.ChatGate(context.Background(), chatID)
	if err != nil {
		// Do not remember a failed read: the chat would pass as a product chat for the cache window.
		slog.Warn("chat profile not readable", "chat", chatID, "error", err)
		return "", nil, time.Time{}
	}
	m.mu.Lock()
	if m.expCache == nil {
		m.expCache = map[string]expGateEntry{}
	}
	m.expCache[chatID] = expGateEntry{model: model, exp: exp, created: created, at: now}
	m.mu.Unlock()
	return model, exp, created
}

// invalidateChatGate forgets the cached profile of a chat.
func (m *Manager) invalidateChatGate(chatID string) {
	m.mu.Lock()
	delete(m.expCache, chatID)
	m.mu.Unlock()
}

// chatBudget is the Budget hook at the LLM proxy: an experiment chat is held to the limits of its run (X3),
// every other chat to the model-cost budget of its autonomous plan (A-E1, D1.9).
func (m *Manager) chatBudget(chatID string) error {
	if _, exp, _ := m.chatGate(chatID); exp != nil {
		return m.experimentLimit(chatID)
	}
	return m.planBudget(chatID)
}

// experimentLimit enforces the limits of an experiment run before a model call is forwarded (X3.2–X3.5). The
// order is duration, cost, tokens; on the first hit the reason is stored at the chat and the chat is aborted.
// Checked before the call: a single call may take the run over the limit, and that is reported as it is.
func (m *Manager) experimentLimit(chatID string) error {
	ctx := context.Background()
	_, exp, created := m.chatGate(chatID)
	if exp == nil {
		return nil
	}
	if exp.Limit != "" {
		return fmt.Errorf("experiment run limit reached: %s", exp.Limit)
	}
	if d := m.expDurationLimit(exp); d > 0 && !created.IsZero() && time.Since(created) >= d {
		return m.hitExperimentLimit(ctx, chatID, ExpLimitDuration)
	}
	cost, tokens, err := m.st.ChatLLMUsage(ctx, chatID)
	if err != nil {
		// An unreadable usage must not block the run: the duration still bounds it.
		slog.Error("experiment run usage not readable", "chat", chatID, "error", err)
		return nil
	}
	if lim := m.expCostLimit(exp); lim > 0 && cost >= lim {
		return m.hitExperimentLimit(ctx, chatID, ExpLimitCost)
	}
	// tokens are input + output without cache reads (X3.3, #206)
	if lim := m.expTokenLimit(exp); lim > 0 && tokens >= lim {
		return m.hitExperimentLimit(ctx, chatID, ExpLimitTokens)
	}
	return nil
}

// expCostLimit, expTokenLimit and expDurationLimit give the limit of the run's profile (#206), else the gateway's
// default from AGW_EXP_MAX_*.
func (m *Manager) expCostLimit(exp *store.Experiment) float64 {
	if exp.Limits != nil && exp.Limits.MaxCostUSD > 0 {
		return exp.Limits.MaxCostUSD
	}
	return m.opt.ExpMaxCostUSD
}

func (m *Manager) expTokenLimit(exp *store.Experiment) int64 {
	if exp.Limits != nil && exp.Limits.MaxTokens > 0 {
		return exp.Limits.MaxTokens
	}
	return m.opt.ExpMaxTokens
}

func (m *Manager) expDurationLimit(exp *store.Experiment) time.Duration {
	if exp.Limits != nil && exp.Limits.MaxDuration != "" {
		if d, err := time.ParseDuration(exp.Limits.MaxDuration); err == nil && d > 0 {
			return d
		}
	}
	return m.opt.ExpMaxDuration
}

// hitExperimentLimit records the reason at the chat and aborts it (X3.5). The first reason wins; a second call
// after the abort reports the stored one.
func (m *Manager) hitExperimentLimit(ctx context.Context, chatID, reason string) error {
	ok, err := m.st.SetExperimentLimit(ctx, chatID, reason)
	switch {
	case err != nil:
		slog.Error("experiment run limit not stored", "chat", chatID, "limit", reason, "error", err)
	case ok:
		m.invalidateChatGate(chatID)
		m.publishChat(ctx, chatID)
		slog.Info("experiment run limit reached", "chat", chatID, "limit", reason)
		go func() {
			actx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			if _, err := m.Abort(actx, chatID); err != nil {
				slog.Warn("experiment run not aborted after the limit", "chat", chatID, "error", err)
			}
		}()
	default:
		// Someone stored a reason in the meantime (or this is not an experiment chat): report the stored one.
		if _, cur, _, err := m.st.ChatGate(ctx, chatID); err == nil && cur != nil && cur.Limit != "" {
			reason = cur.Limit
		}
	}
	return fmt.Errorf("experiment run limit reached: %s", reason)
}

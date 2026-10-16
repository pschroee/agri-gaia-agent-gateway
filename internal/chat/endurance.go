// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

// Autonomy A-E3: an autonomous plan runs for hours, long after the user closed the browser. Two things keep
// it alive. The gateway renews the owner's login ahead of time while a plan of theirs is approved (D3.1), so
// neither the access token nor the Keycloak session behind it ends between two steps. And when that login is
// gone anyway — for instance after a restart, which loses the sessions (D3.2) — the plan is paused with
// reason "login" instead of letting its steps fail (D3.5); the owner's next login continues it automatically
// (D3.6). The restart itself is handled by the store (RecoverPlans, D3.4).
//
// What this file does not do: it never stores a refresh token (D3.2) and it never touches a plan of another
// owner (the store's queries join chats.owner, so a login only ever continues the user's own plans).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agw/internal/oidc"
	"agw/internal/plan"
	"agw/internal/store"
)

const (
	// planKeepAliveTick: how often the login of the owners of running plans is renewed (D3.1). Four minutes
	// stay well below Keycloak's SSO session idle (30 minutes in the default realm), also while a plan only
	// watches a job for an hour and no browser is open.
	planKeepAliveTick = 4 * time.Minute
	// planSessionHeadroom: an autonomous plan ends this long before the login session that carries its calls
	// ends (D3.3), so it never runs into the session's end.
	planSessionHeadroom = time.Hour
)

// SessionKeeper is the login side of the gateway as the manager needs it (A-E3): the oidc service when the
// gateway logs users in through the platform, nil in the token mode (there is no session to keep fresh and
// none to continue a plan with).
type SessionKeeper interface {
	// KeepFresh renews the user's session if its access token is close to expiry; oidc.ErrNoSession when the
	// user has no live session any more.
	KeepFresh(ctx context.Context, sub string) error
	// SessionEnds reports when the user's newest session ends; false without one (D3.3).
	SessionEnds(sub string) (time.Time, bool)
	// OnSession registers the callback for a new login of a user (D3.6); it replaces the previous one.
	OnSession(f func(sub string))
}

// RunPlanEndurance keeps the login of every user with a running autonomous plan fresh (D3.1) and continues
// their plans when a login comes back (D3.6). It runs until ctx is done (the orchestrator's lifetime).
func (m *Manager) RunPlanEndurance(ctx context.Context) {
	sk := m.opt.Sessions
	if sk == nil {
		slog.Info("plan endurance off (no login through the platform)")
		return
	}
	sk.OnSession(func(sub string) { m.resumeLoginPlans(context.Background(), sub) })
	t := time.NewTicker(planKeepAliveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.keepPlansLoggedIn(ctx)
		}
	}
}

// keepPlansLoggedIn renews the login of every owner with an approved autonomous plan, once (D3.1). An owner
// without a live session does not let the plan's steps fail: the plan is paused (D3.5).
func (m *Manager) keepPlansLoggedIn(ctx context.Context) {
	sk := m.opt.Sessions
	if sk == nil {
		return
	}
	owners, err := m.st.AutonomousPlanOwners(ctx)
	if err != nil {
		slog.Warn("owners of running plans not readable", "error", err)
		return
	}
	for _, o := range owners {
		switch err := sk.KeepFresh(ctx, o.Owner); {
		case err == nil:
		case errors.Is(err, oidc.ErrNoSession):
			for _, id := range o.Plans {
				m.pausePlanForLogin(ctx, id)
			}
		default:
			// Keycloak unreachable or similar: the session may still be there, so the plan keeps running and
			// the next tick tries again.
			slog.Warn("login of a plan owner not renewed", "owner", o.Owner, "error", err)
		}
	}
}

// pausePlanForLogin pauses an approved autonomous plan because its owner's login is gone (D3.1/D3.5). That is
// not a failure of the plan: the owner's next login continues it (D3.6). The user hears about it through an
// "attention" event, which the panel shows without the chat being open.
func (m *Manager) pausePlanForLogin(ctx context.Context, planID string) {
	p, err := m.st.GetPlan(ctx, planID)
	if err != nil {
		slog.Error("plan of a lost login not readable", "plan", planID, "error", err)
		return
	}
	if p.State != plan.StateApproved {
		return // ended, or paused by the user (or an earlier tick) in the meantime
	}
	ok, err := m.st.PausePlan(ctx, planID, store.PauseLogin)
	if err != nil {
		slog.Error("plan not paused after the login was gone", "chat", p.ChatID, "plan", planID, "error", err)
		return
	}
	if !ok {
		return
	}
	m.publishPlan(ctx, planID)
	m.publish(p.ChatID, Event{Kind: "attention", Data: map[string]string{
		"chat_id": p.ChatID, "plan_id": planID, "reason": store.PauseLogin,
		"message": "the login of the plan's owner is gone; the plan is paused and continues as soon as the user is logged in to the platform again"}})
	slog.Info("plan paused, the owner's login is gone", "chat", p.ChatID, "plan", planID)
}

// resumeLoginPlans continues the plans that were paused because the owner's login was gone (D3.6): the new
// session is exactly the repair they waited for, so they continue without the user doing anything else. Which
// plans those are is the store's answer — they hang on the chat's owner, so a login never continues a plan of
// another user. Plans paused for any other reason (restart, user, abort, budget) stay paused: the user
// decides about those.
func (m *Manager) resumeLoginPlans(ctx context.Context, sub string) {
	owners, err := m.st.LoginPausedPlans(ctx, sub)
	if err != nil {
		slog.Warn("paused plans of a new login not readable", "user", sub, "error", err)
		return
	}
	for _, o := range owners {
		for _, id := range o.Plans {
			p, err := m.st.GetPlan(ctx, id)
			if err != nil {
				slog.Error("paused plan not readable", "plan", id, "error", err)
				continue
			}
			if p.State != plan.StatePaused || p.PauseReason != store.PauseLogin {
				continue
			}
			ok, err := m.st.ResumePlan(ctx, id)
			if err != nil || !ok {
				slog.Error("plan not continued after the login came back", "chat", p.ChatID, "plan", id, "ok", ok, "error", err)
				continue
			}
			m.publishPlan(ctx, id)
			note := fmt.Sprintf("The user is logged in to the platform again; plan %q continues with step %d. Keep going and report when it is through.",
				id, nextStepIdx(p))
			if _, err := m.st.EnqueueSystem(ctx, p.ChatID, store.NotePlan, []string{id}, note); err != nil {
				slog.Error("resume note not queued", "chat", p.ChatID, "plan", id, "error", err)
			} else {
				m.publishQueue(ctx, p.ChatID, "queued", nil, "")
			}
			// The chat may be resting: an autonomous plan wakes it (D2.6), and a live one takes the note now.
			m.wakeIfLive(p.ChatID)
			m.wakeDormant(p.ChatID, id)
			slog.Info("plan continued, the owner's login is back", "chat", p.ChatID, "plan", id)
		}
	}
}

// loginGone reports whether a platform call failed because the user's login was gone (D3.5). The error comes
// from the token exchange's subject token (platform.Config.Subject → oidc.AccessToken).
func loginGone(err error) bool { return err != nil && errors.Is(err, oidc.ErrNoSession) }

// planSessionEnd is the latest end an autonomous plan may have, given the login session of the chat's owner
// (D3.3): the end of that session minus planSessionHeadroom, so the plan never runs into the session's end.
// ok is false when no session bounds the plan — the token mode, a chat without an owner, or an owner without
// a live session; then the plan's own duration is the only bound.
func (m *Manager) planSessionEnd(ctx context.Context, chatID string) (time.Time, bool) {
	if m.opt.Sessions == nil {
		return time.Time{}, false
	}
	owner, err := m.st.ChatOwner(ctx, chatID)
	if err != nil {
		slog.Warn("owner of a chat not readable for the plan's duration", "chat", chatID, "error", err)
		return time.Time{}, false
	}
	if owner == "" {
		return time.Time{}, false
	}
	end, ok := m.opt.Sessions.SessionEnds(owner)
	if !ok {
		return time.Time{}, false
	}
	return end.Add(-planSessionHeadroom), true
}

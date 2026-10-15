// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"agw/internal/pool"
	"agw/internal/store"
)

// ErrTrashed: the chat is in the trash; it reads, but takes no message, no wake-up, no resume and no approval
// (issue #76).
var ErrTrashed = errors.New("chat is in the trash")

// ErrNotInTrash: only a chat in the trash can be restored or deleted for good (issue #76).
var ErrNotInTrash = errors.New("chat is not in the trash")

// trashRefusal is the text of the refusals that the agent sees (platform calls, plans, uploads, internet).
const trashRefusal = "chat is in the trash"

// trashNow is the clock of the trash: the time of the move and the base of the retention; tests replace it.
var trashNow = time.Now

// inTrash reports whether a chat is in the trash. Unknown and failing chats count as not trashed; the
// caller's own read reports them.
func (m *Manager) inTrash(ctx context.Context, chatID string) bool {
	t, err := m.st.ChatTrashed(ctx, chatID)
	return err == nil && t
}

// Trash moves a chat into the trash (issue #76). The order stops everything that works for the chat, and
// the trash comes first: from the moment trashed_at is set, every route that would start work refuses
// (ensureLive, Resume, the queue, PlatformCall, ProposePlan, Upload, RequestInternet). Idempotent: a chat
// already in the trash comes back unchanged.
func (m *Manager) Trash(ctx context.Context, chatID string) (ChatView, error) {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	if c.TrashedAt != nil {
		return m.view(c), nil
	}
	// The body is a state change: it finishes even when the request is gone.
	ctx = context.WithoutCancel(ctx)
	// Under the queue lock, so that a message which checked before cannot enter the queue afterwards.
	unqueue := m.queueLock(chatID)
	moved, err := m.st.MoveToTrash(ctx, chatID, trashNow())
	if err == nil && moved {
		_, err = m.st.DeleteUndelivered(ctx, chatID)
	}
	unqueue()
	if err != nil {
		return ChatView{}, err
	}
	if !moved {
		return m.View(ctx, chatID)
	}
	// The notebook server of the chat goes with it (own code C-E3). Its volume stays, so the files of
	// earlier runs are there again when the chat comes back; a server nothing touches would also be
	// stopped by the reaper. Only the error is logged: the chat is in the trash either way.
	if m.opt.Notebook != nil {
		if err := m.opt.Notebook.StopChat(ctx, chatID); err != nil {
			slog.Warn("notebook server not stopped with the trash", "chat", chatID, "error", err)
		}
	}
	// Open approvals (also of plans) are rejected: the waiting calls end with "rejected".
	m.rejectPending(ctx, chatID, trashRefusal)
	if n, err := m.st.ExpireOpenPlans(ctx, chatID); err != nil {
		slog.Warn("plans not expired", "chat", chatID, "error", err)
	} else if n > 0 {
		slog.Info("plans expired with the trash", "chat", chatID, "count", n)
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		// The running turn stops; Abort also holds back queued messages (none can come in now).
		if _, err := m.Abort(ctx, chatID); err != nil {
			slog.Warn("abort before the trash failed", "chat", chatID, "error", err)
		}
	}
	ended, err := m.st.StopRunningBackground(ctx, chatID, trashRefusal)
	if err != nil {
		slog.Warn("background tasks not stopped", "chat", chatID, "error", err)
	}
	for _, t := range ended {
		m.publishBackground(chatID, "ended", t)
	}
	m.mu.Lock()
	l = m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		// Session and workspace stay for the restore; the slot goes back to the pool (with its subagents).
		m.detach(ctx, chatID, l, true, true, pool.ReasonTrashed)
	}
	// Messages that the abort handed back to the queue must not wait for a restore (they would go out with
	// the next message, and the user has moved the chat away).
	if _, err := m.st.DeleteUndelivered(ctx, chatID); err != nil {
		slog.Warn("queue not cleared", "chat", chatID, "error", err)
	}
	slog.Info("chat moved to the trash", "chat", chatID, "background", len(ended))
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// Restore takes a chat out of the trash. It does not wake the chat: the UI resumes it when it is opened, as
// before (issue #31). Approvals stay rejected and plans expired.
func (m *Manager) Restore(ctx context.Context, chatID string) (ChatView, error) {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	if c.TrashedAt == nil {
		return ChatView{}, ErrNotInTrash
	}
	ok, err := m.st.Restore(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	if !ok {
		return ChatView{}, ErrNotInTrash
	}
	slog.Info("chat restored from the trash", "chat", chatID)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// DeletePermanently deletes a chat that is in the trash for good (issue #76). Anything else is ErrNotInTrash.
func (m *Manager) DeletePermanently(ctx context.Context, chatID string) error {
	_, err := m.deleteFromTrash(ctx, chatID, time.Time{})
	return err
}

// deleteFromTrash deletes a chat in the trash, under the chat lock. With before set, only a chat that went into the
// trash before that time (retention); it reports false for a chat that does not qualify.
func (m *Manager) deleteFromTrash(ctx context.Context, chatID string, before time.Time) (bool, error) {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return false, err
	}
	if c.TrashedAt == nil {
		return false, ErrNotInTrash
	}
	if !before.IsZero() && !c.TrashedAt.Before(before) {
		return false, nil
	}
	ctx = context.WithoutCancel(ctx)
	// Nothing should be live (the trash stops the chat); if something is, it goes without a save.
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		m.detach(ctx, chatID, l, false, false, pool.ReasonTrashed)
	}
	// The object keys are read first: the cascade removes the rows that name them.
	keys, err := m.st.ChatBlobKeys(ctx, chatID)
	if err != nil {
		return false, err
	}
	tomb, err := m.st.DeleteChatAnonymised(ctx, chatID)
	if err != nil {
		return false, err
	}
	failed := 0
	for _, k := range keys {
		if err := m.blobs.Delete(ctx, k); err != nil {
			failed++
			slog.Warn("object not deleted", "chat", chatID, "key", k, "error", err)
		}
	}
	slog.Info("chat deleted permanently", "chat", chatID, "tombstone", tomb, "blobs", len(keys), "failed", failed)
	return true, nil
}

// TrashList returns the chats in the trash of an owner (nil: all, token mode).
func (m *Manager) TrashList(ctx context.Context, owner *string) ([]store.TrashItem, error) {
	return m.st.ListTrash(ctx, owner)
}

// EmptyTrash deletes every chat of an owner (nil: all) that is in the trash, for good. A failing chat does not
// stop the others; the count is what was deleted, the error the first failure.
func (m *Manager) EmptyTrash(ctx context.Context, owner *string) (int, error) {
	items, err := m.st.ListTrash(ctx, owner)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var first error
	for _, it := range items {
		ok, err := m.deleteFromTrash(ctx, it.ID, time.Time{})
		if errors.Is(err, ErrNotInTrash) {
			continue // restored meanwhile
		}
		if err != nil {
			slog.Warn("chat not deleted from the trash", "chat", it.ID, "error", err)
			if first == nil {
				first = err
			}
			continue
		}
		if ok {
			deleted++
		}
	}
	return deleted, first
}

// RunTrashRetention deletes the chats that stayed in the trash longer than AGW_TRASH_RETENTION_DAYS. Off (0)
// it returns at once and never deletes. Otherwise it runs at the start and then hourly until ctx ends.
func (m *Manager) RunTrashRetention(ctx context.Context) {
	days := m.opt.TrashRetentionDays
	if days <= 0 {
		return
	}
	purge := func() {
		cutoff := trashNow().Add(-time.Duration(days) * 24 * time.Hour)
		ids, err := m.st.TrashedBefore(ctx, cutoff)
		if err != nil {
			slog.Warn("trash retention", "error", err)
			return
		}
		n := 0
		for _, id := range ids {
			ok, err := m.deleteFromTrash(ctx, id, cutoff)
			if err != nil {
				slog.Warn("trash retention: chat not deleted", "chat", id, "error", err)
				continue
			}
			if ok {
				n++
			}
		}
		if n > 0 {
			slog.Info("trash retention deleted", "deleted", n, "days", days)
		}
	}
	purge()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}

// Trashed reports whether a chat is in the trash (unknown chats: store.ErrNotFound).
func (m *Manager) Trashed(ctx context.Context, chatID string) (bool, error) {
	return m.st.ChatTrashed(ctx, chatID)
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"agw/internal/store"
)

// The reaper stops the notebook server of a chat that had no run for AGW_NOTEBOOK_IDLE_MIN minutes.
// The server holds 2 CPUs and 8 GB of the instance for nothing once the work is done; its volume
// stays, so the files of earlier runs remain the work of the user. The reaper does not touch the
// runs themselves: the status call and the watcher record an end (they see the exit code in the run
// directory), the store keeps what the run itself wrote. A stop is recorded on the chat's last run row,
// so the server is stopped once; the next run is a new row and makes the server a candidate again.
// A server the user works in (its last activity in the hub is newer than the idle time) is kept.
// A run holds the chat's lock (chatLock) while it starts; the reaper only stops under that lock.
//
// How often the reaper looks; a variable, so that a test does not wait.
var reaperEvery = 5 * time.Minute

// RunReaper stops idle notebook servers until the context ends. It does nothing when the idle time
// is 0 (AGW_NOTEBOOK_IDLE_MIN) or notebooks are not configured.
func (n *Notebooks) RunReaper(ctx context.Context) {
	if n == nil || n.hub == nil || n.cfg.APIURL == "" || n.cfg.IdleMin <= 0 {
		return
	}
	slog.Info("notebook reaper started", "idle_minutes", n.cfg.IdleMin)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(reaperEvery):
		}
		if err := n.StopIdle(ctx); err != nil {
			slog.Warn("notebook reaper failed", "error", err)
		}
	}
}

// StopIdle stops every notebook server whose last run ended before the idle time and which has no
// run going. An idle time of 0 (AGW_NOTEBOOK_IDLE_MIN, never stop) stops nothing.
func (n *Notebooks) StopIdle(ctx context.Context) error {
	if n == nil || n.hub == nil || n.cfg.APIURL == "" || n.cfg.IdleMin <= 0 {
		return nil
	}
	before := n.now().Add(-time.Duration(n.cfg.IdleMin) * time.Minute)
	servers, err := n.store.IdleNotebookServers(ctx, before)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if err := n.stopOne(ctx, s, before); err != nil {
			return err
		}
	}
	return nil
}

// stopOne stops one idle server under the chat's lock. A chat whose lock is taken has a run that
// is starting; it is skipped, and the next round sees its new run row. Under the lock the chat is
// checked again, because a run may have begun and written its row since the list was read.
func (n *Notebooks) stopOne(ctx context.Context, s store.NotebookServer, before time.Time) error {
	mu := n.chatLock(s.ChatID)
	if !mu.TryLock() {
		slog.Debug("idle notebook server skipped, a run is starting", "chat", s.ChatID, "server", s.Server)
		return nil
	}
	defer mu.Unlock()
	idle, err := n.store.NotebookServerIdle(ctx, s.ChatID, s.RunID, before)
	if err != nil {
		return err
	}
	if !idle {
		return nil
	}
	info, err := n.hub.ServerInfo(ctx, s.HubUser, s.Server)
	state := info.State
	switch {
	case errors.Is(err, ErrNoHubUser):
		state = stateNotStarted
	case err != nil:
		slog.Warn("idle notebook server state unknown", "chat", s.ChatID, "server", s.Server, "error", err)
		return nil
	}
	// "stopping" counts like "running": the stop is asked again (the hub answers 202) and recorded.
	running := state != stateNotStarted
	if running && info.LastActivity.After(before) {
		slog.Info("idle notebook server kept, used in JupyterHub", "chat", s.ChatID, "server", s.Server, "last_activity", info.LastActivity)
		return nil
	}
	if running {
		if err := n.hub.Stop(ctx, s.HubUser, s.Server); err != nil {
			slog.Warn("idle notebook server not stopped", "chat", s.ChatID, "server", s.Server, "error", err)
			return nil
		}
	}
	if err := n.store.MarkNotebookServerStopped(ctx, s.RunID, n.now()); err != nil {
		return err
	}
	if running {
		slog.Info("idle notebook server stopped", "chat", s.ChatID, "server", s.Server)
	} else {
		slog.Debug("idle notebook server was not running", "chat", s.ChatID, "server", s.Server)
	}
	return nil
}

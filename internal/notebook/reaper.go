// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The reaper stops the notebook server of a chat that had no run for AGW_NOTEBOOK_IDLE_MIN minutes.
// The server holds 2 CPUs and 8 GB of the instance for nothing once the work is done; its volume
// stays, so the files of earlier runs remain the work of the user. The reaper does not touch the
// runs themselves: the status call and the watcher record an end (they see the exit code in the run
// directory), the store keeps what the run itself wrote. A stop is recorded on the chat's last run row,
// so the server is stopped once; the next run is a new row and makes the server a candidate again.
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
		state, err := n.hub.ServerState(ctx, s.HubUser, s.Server)
		switch {
		case errors.Is(err, ErrNoHubUser):
			state = "not started"
		case err != nil:
			slog.Warn("idle notebook server state unknown", "chat", s.ChatID, "server", s.Server, "error", err)
			continue
		}
		running := state != "not started"
		if running {
			if err := n.hub.Stop(ctx, s.HubUser, s.Server); err != nil {
				slog.Warn("idle notebook server not stopped", "chat", s.ChatID, "server", s.Server, "error", err)
				continue
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
	}
	return nil
}

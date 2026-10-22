// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// observedReadNote is the sentence the answer of a read of an observed job adds (#212): the same in every binding
// (CLI and MCP in the text of the answer, REST in X-Agw-Note).
const observedReadNote = "The gateway observes this job (job %s) and tells you when it ends; do not poll it — end your turn. " +
	"Reads of an observed job are answered at most every 15 s."

// observedReadSpacing is the least time between two equal reads of the same observed job in one chat. It bounds the
// cost of a loop of reads (701 reads of one build in 10 minutes in the live run of 22.10.2026) when the agent
// ignores the note.
const observedReadSpacing = 15 * time.Second

// observedNow and observedSleep are the clock and the wait of the spacing; tests replace them.
var (
	observedNow   = time.Now
	observedSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// observedReads remembers when an equal read of an observed job was last let through.
type observedReads struct {
	mu sync.Mutex
	at map[string]time.Time
}

func readKey(chatID, jobID string, req platform.Request) string {
	return chatID + "\x00" + jobID + "\x00" + req.Method + " " + req.Path
}

// slot reserves the time at which the read may go out and returns how long to wait for it. Concurrent equal reads
// queue one behind the other.
func (o *observedReads) slot(key string) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.at == nil {
		o.at = map[string]time.Time{}
	}
	now := observedNow()
	at := now
	if last, ok := o.at[key]; ok && last.Add(observedReadSpacing).After(now) {
		at = last.Add(observedReadSpacing)
	}
	o.at[key] = at
	return at.Sub(now)
}

// forget drops the entries of a job that ended.
func (o *observedReads) forget(chatID, jobID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	prefix := chatID + "\x00" + jobID + "\x00"
	for k := range o.at {
		if strings.HasPrefix(k, prefix) {
			delete(o.at, k)
		}
	}
}

func (m *Manager) forgetObservedReads(chatID, jobID string) { m.obsReads.forget(chatID, jobID) }

// observedRead finds the running job of the chat that a read of a job polls (GET /tasks/{id}, GET
// /train/containers/{id}/status|logs|metrics). The trials of a search are left out: the search has curves of its
// own and notes of its own.
func (m *Manager) observedRead(ctx context.Context, chatID string, req platform.Request) (jobID string, ok bool) {
	kind, id, ok := watch.ReadOf(req)
	if !ok {
		return "", false
	}
	jobs, err := m.st.ChatWatchJobs(ctx, chatID)
	if err != nil {
		return "", false
	}
	for _, j := range jobs {
		if j.State == store.JobRunning && !isTrialJob(j.ID) && watch.ReadMatches(j.Kind, j.Ref, kind, id) {
			return j.ID, true
		}
	}
	return "", false
}

// spaceObservedRead waits until the read may go out (observedReadSpacing after the last equal one). The wait ends
// with ctx.
func (m *Manager) spaceObservedRead(ctx context.Context, chatID, jobID string, req platform.Request) error {
	d := m.obsReads.slot(readKey(chatID, jobID, req))
	if d <= 0 {
		return nil
	}
	slog.Debug("observed read spaced", "chat", chatID, "job", jobID, "path", req.Path, "wait", d)
	return observedSleep(ctx, d)
}

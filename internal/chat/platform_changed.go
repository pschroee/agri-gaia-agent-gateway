// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"time"

	"agw/internal/delegation"
	"agw/internal/watch"
)

// KindPlatformChanged is the SSE event "platform_changed" (issue #167): the agent changed something on
// the platform, or an observed job handed over or ended. It also goes to the stream across chats
// (crossChatKinds), so open platform pages reload whatever chat or tab the change came from.
const KindPlatformChanged = "platform_changed"

// Sources of a platform_changed event.
const (
	ChangeSourceCall = "call" // a successful writing call of the agent
	ChangeSourceJob  = "job"  // an observed job handed over or ended
	ChangeSourceStop = "stop" // the user stopped a job in the UI
)

// PlatformChangedEvent is the data of the event: what changed, never a body or an answer.
type PlatformChangedEvent struct {
	ChatID  string              `json:"chat_id"`
	Source  string              `json:"source"`
	Path    string              `json:"path"`
	JobID   string              `json:"job_id"`
	Changes []delegation.Change `json:"changes"`
	At      time.Time           `json:"at"`
}

// PublishPlatformChanged sends the event; nothing goes out for an empty list of changes.
func (m *Manager) PublishPlatformChanged(chatID string, ev PlatformChangedEvent) {
	if len(ev.Changes) == 0 {
		return
	}
	ev.ChatID = chatID
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	m.publish(chatID, Event{Kind: KindPlatformChanged, Data: ev})
}

// jobChanges lists what a job changed: its kind's resource ("run" when the job is handed over to the
// kind, "update" when it ended) and every output as a created object. A kind without a resource gives only
// the outputs.
func jobChanges(kind watch.Kind, p watch.Progress, ended bool) []delegation.Change {
	var out []delegation.Change
	if kind.Resource != "" {
		action := delegation.Run
		if ended {
			action = delegation.Update
		}
		out = append(out, delegation.Change{Resource: kind.Resource, Action: action})
	}
	for _, o := range p.Outputs {
		if o.Resource == "" {
			continue
		}
		out = append(out, delegation.Change{Resource: o.Resource, Action: delegation.Create, ID: o.ID})
	}
	return out
}

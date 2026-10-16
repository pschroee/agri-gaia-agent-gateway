// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"log/slog"

	"agw/internal/artifacts"
	"agw/internal/store"
)

// RemoveInput deletes an input the user uploaded but did not send with a message (issue L4/#121): the row, the
// file in the object store and the mirror in the sandbox of an active chat. The input goes as `artifact_removed`
// to the chat's subscribers, so a second view (the panel next to the agent page) drops its card as well.
//
// A file that went with a message stays (store.ErrAlreadySent): it belongs to the history and removing it would
// take the attachment away from an earlier message. An unknown name is store.ErrNotFound.
func (m *Manager) RemoveInput(ctx context.Context, chatID, name string) error {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return err
	}
	if c.TrashedAt != nil {
		return ErrTrashed
	}
	sn := artifacts.SanitizeName(name)
	if sn == "" || sn != name {
		return fmt.Errorf("%w: invalid attachment %q", ErrInvalid, name)
	}
	art, err := m.st.DeleteUnsentInput(ctx, chatID, sn)
	if err != nil {
		return err
	}
	// From here on the input is gone from the list; the rest is cleanup whose failure is only logged (the file
	// no longer shows up and cannot be sent any more, a remainder in the sandbox is not visible to the user).
	if err := m.blobs.Delete(ctx, art.ObjectKey); err != nil {
		slog.Warn("input not removed from the object store", "chat", chatID, "name", sn, "error", err)
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		if _, err := l.slot.Worker.Exec(ctx, []string{"rm", "-f", "--", inputsDir + "/" + sn}, nil); err != nil {
			slog.Warn("input not removed from the sandbox", "chat", chatID, "name", sn, "error", err)
		}
	}
	m.publish(chatID, Event{Kind: "artifact_removed", Data: map[string]string{"kind": store.KindInput, "name": sn}})
	m.publishChat(ctx, chatID)
	return nil
}

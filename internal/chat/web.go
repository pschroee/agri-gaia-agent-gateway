package chat

import (
	"context"
	"log/slog"

	"agw/internal/store"
	"agw/internal/webproxy"
)

// WebAccess assigns a request at the web proxy to its chat (webproxy.Gate): by the address of pi's
// container in the slot network, as at the LLM proxy. internet is the chat's switch.
func (m *Manager) WebAccess(ip string) (chat, slot string, internet bool) {
	if ip == "" {
		return "", "", false
	}
	m.mu.Lock()
	for id, l := range m.live {
		if l.ip == ip {
			chat, slot = id, l.slot.ID
			break
		}
	}
	m.mu.Unlock()
	if chat == "" {
		return "", "", false
	}
	return chat, slot, m.InternetOn(context.Background(), chat)
}

// InternetOn: does the chat have internet? (also sock.InternetStater)
func (m *Manager) InternetOn(ctx context.Context, chatID string) bool {
	on, err := m.st.ChatInternet(ctx, chatID)
	return err == nil && on
}

func webRow(r webproxy.Request) store.WebRequest {
	return store.WebRequest{ChatID: r.ChatID, SlotID: r.SlotID, SourceIP: r.SourceIP, Method: r.Method, Host: r.Host, Port: r.Port,
		Path: r.Path, Status: r.Status, BytesUp: r.BytesUp, BytesDown: r.BytesDown, Denied: r.Denied, StartedAt: r.StartedAt, DurationMs: r.DurationMs}
}

// RecordWeb stores a request at the web proxy (webproxy.Gate) and reports it to the UI.
func (m *Manager) RecordWeb(r webproxy.Request) int64 {
	wr := webRow(r)
	id, err := m.st.AddWebRequest(context.Background(), wr)
	if err != nil {
		slog.Warn("web request not stored", "chat", r.ChatID, "err", err)
		return 0
	}
	wr.ID = id
	m.publish(r.ChatID, Event{Kind: "web_request", Data: wr})
	return id
}

// FinishWeb adds bytes and duration to a tunnel when it closes (webproxy.Gate).
func (m *Manager) FinishWeb(id int64, r webproxy.Request) {
	if id == 0 {
		return
	}
	if err := m.st.FinishWebRequest(context.Background(), id, r.BytesUp, r.BytesDown, r.DurationMs, r.Denied); err != nil {
		slog.Warn("web request not updated", "chat", r.ChatID, "err", err)
	}
}

// WebRequests returns a chat's requests through the web proxy.
func (m *Manager) WebRequests(ctx context.Context, chatID string) ([]store.WebRequest, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return nil, err
	}
	return m.st.WebRequests(ctx, chatID)
}

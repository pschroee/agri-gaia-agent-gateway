package chat

import (
	"context"
	"log/slog"

	"agw/internal/store"
	"agw/internal/webproxy"
)

// InternetOn: does the chat have internet, i.e. is any internet grant active? (also sock.InternetStater; the flag is
// derived from the grants, issue #175)
func (m *Manager) InternetOn(ctx context.Context, chatID string) bool {
	on, err := m.st.ChatInternet(ctx, chatID)
	return err == nil && on
}

func webRow(r webproxy.Request) store.WebRequest {
	return store.WebRequest{ChatID: r.ChatID, SlotID: r.SlotID, SourceIP: r.SourceIP, Method: r.Method, Host: r.Host, Port: r.Port,
		Path: r.Path, Status: r.Status, BytesUp: r.BytesUp, BytesDown: r.BytesDown, Denied: r.Denied, StartedAt: r.StartedAt, DurationMs: r.DurationMs,
		GrantID: r.GrantID, Origin: r.Origin, ToolCallID: r.ToolCallID}
}

// RecordWeb stores a request at the web proxy (webproxy.Gate) and reports it to the UI. A refusal counts on the
// grant that governs the chat.
func (m *Manager) RecordWeb(r webproxy.Request) int64 {
	wr := webRow(r)
	id, err := m.st.AddWebRequestCapped(context.Background(), wr, m.internetLogRows())
	if err != nil {
		slog.Warn("web request not stored", "chat", r.ChatID, "err", err)
		return 0
	}
	wr.ID = id
	m.publish(r.ChatID, Event{Kind: "web_request", Data: wr})
	if r.Denied != "" && r.GrantID > 0 {
		if l := m.grants.get(r.GrantID); l != nil {
			l.blocked.Add(1)
		}
		if err := m.st.BumpBlocked(context.Background(), r.GrantID); err != nil {
			slog.Warn("blocked count not stored", "grant", r.GrantID, "err", err)
		}
	}
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

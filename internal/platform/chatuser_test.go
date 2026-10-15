// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"net/http/httptest"
	"testing"
)

// ChatUser names the user whose token the chat uses (G1.3): with token exchange the exchanged
// token's preferred_username (the subject stays the user), without exchange the configured account's.
func TestChatUser(t *testing.T) {
	ctx := context.Background()
	f := &fakeKeycloak{}
	c, _ := exchangeClient(t, f)
	if got, err := c.ChatUser(ctx, "chat-a"); err != nil || got != "test" {
		t.Fatalf("with exchange: %q %v", got, err)
	}
	if len(f.exchanges) != 1 {
		t.Fatalf("exchanges: %v", f.exchanges)
	}
	// The second call reuses the chat's token: no further exchange.
	if got, err := c.ChatUser(ctx, "chat-a"); err != nil || got != "test" {
		t.Fatalf("second call: %q %v", got, err)
	}
	if len(f.exchanges) != 1 {
		t.Fatalf("second call exchanged again: %v", f.exchanges)
	}
	// Without a chat there is no exchanged token.
	if _, err := c.ChatUser(ctx, ""); err == nil {
		t.Error("without a chat expected an error")
	}

	// Without exchange the configured account's token speaks.
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	plain, err := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "agw-agent", ClientSecret: "secret", User: "test", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plain.ChatUser(ctx, "chat-b"); err != nil || got != "test" {
		t.Fatalf("without exchange: %q %v", got, err)
	}
	// A client that is not configured at all reports that.
	var nilClient *Client
	if _, err := nilClient.ChatUser(ctx, "chat-a"); err == nil {
		t.Error("unconfigured client expected an error")
	}
}

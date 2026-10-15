// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"errors"
)

// ChatUser names the user whose rights the chat's platform calls carry (G1.3): the
// preferred_username of the token the chat uses for platform calls. With token exchange that is the
// exchanged token (sub stays the user, azp names the agent); without exchange it is the user's own
// token, and with the password grant of the configured account that account's name. chats.owner is
// not a substitute: it holds the sub, not the name.
func (c *Client) ChatUser(ctx context.Context, chatID string) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}
	tok, err := c.tokenFor(ctx, chatID, c.cfg.Audiences, false)
	if err != nil {
		return "", err
	}
	cl, err := ParseClaims(tok)
	if err != nil {
		return "", err
	}
	if cl.Username == "" {
		return "", errors.New("platform token without preferred_username")
	}
	return cl.Username, nil
}

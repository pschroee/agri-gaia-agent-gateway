// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	"agw/internal/config"
)

// X4: the fixtures log in as their experiment accounts with the public client of X1.2. agw-agent is
// confidential; a password grant without its client secret is refused as unauthorized_client.
func TestFixtureClientUsesFrontend(t *testing.T) {
	env := config.Env{
		PlatformAPIURL:   "https://api.example.org",
		PlatformTokenURL: "https://keycloak.example.org/realms/test/protocol/openid-connect/token",
		PlatformClientID: "agw-agent",
		ExpClientID:      "frontend",
	}
	c, err := fixtureClient(env, "agent-exp", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("no client")
	}
	if got := c.Info().ClientID; got != "frontend" {
		t.Fatalf("client %q, want frontend", got)
	}
	if c.Exchanging() {
		t.Fatal("the fixtures do not exchange tokens")
	}
	if c, err := fixtureClient(env, "", ""); err != nil || c != nil {
		t.Fatalf("account without credentials: %v %v", c, err)
	}
	if got := expClientSecret(env); got != "" {
		t.Fatalf("public client took the confidential secret")
	}
	// With token exchange the account logs in through agw-agent; its secret is the gateway's.
	env.ExpClientID, env.PlatformClientSecret = "agw-agent", "client-secret"
	if got := expClientSecret(env); got != "client-secret" {
		t.Fatalf("confidential client without its secret")
	}
	c, err = fixtureClient(env, "agent-exp", "secret")
	if err != nil || c == nil || c.Info().ClientID != "agw-agent" {
		t.Fatalf("confidential fixture client: %v %v", c, err)
	}
}

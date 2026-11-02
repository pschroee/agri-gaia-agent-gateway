// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"agw/internal/config"
	"agw/internal/oidc"
	"agw/internal/platform"
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

// BM1: the runner reads the base datasets before a run's chat exists. With a per-chat login that call names the
// stand-in chat, and the token comes from the experiment account without a lookup in the store.
func TestOwnerTokenForTheExperimentRunner(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	srv = httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"token_endpoint":%q,"authorization_endpoint":"x","jwks_uri":"x"}`, srv.URL, srv.URL+"/token")
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"tok-exp","expires_in":300}`)
	})
	exp := oidc.NewPasswordSource(srv.URL, "frontend", "", "agent-exp", "pw")
	// st and auth stay nil: the stand-in chat must not reach them.
	tok, err := ownerToken(nil, nil, exp)(context.Background(), platform.ExpRunnerChat)
	if err != nil || tok != "tok-exp" {
		t.Fatalf("token %q, err %v", tok, err)
	}
}

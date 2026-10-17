// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJWT builds a token whose payload carries sub; the signature is irrelevant here because the
// source reads the subject from the token it just received from the fake endpoint.
func fakeJWT(t *testing.T, sub string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "RS256"}) + "." + enc(map[string]string{"sub": sub, "preferred_username": "agent-exp"}) + ".sig"
}

type fakeAccount struct {
	srv     *httptest.Server
	sub     string
	ttl     int
	status  int
	hit     atomic.Int32
	forms   []string
	secrets []string
	enabled atomic.Bool
}

// startAccount simulates the Keycloak endpoints the source uses: discovery and the token endpoint.
func startAccount(t *testing.T) *fakeAccount {
	t.Helper()
	f := &fakeAccount{sub: "sub-exp", ttl: 300, status: http.StatusOK}
	f.enabled.Store(true)
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"token_endpoint":%q,"authorization_endpoint":"x","jwks_uri":"x"}`, f.srv.URL, f.srv.URL+"/token")
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.hit.Add(1)
		f.forms = append(f.forms, r.Form.Get("grant_type")+" "+r.Form.Get("client_id")+" "+r.Form.Get("username"))
		f.secrets = append(f.secrets, r.Form.Get("client_secret"))
		if !f.enabled.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			fmt.Fprint(w, `{"error":"unauthorized_client","error_description":"direct access not allowed"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":%d,"token_type":"Bearer"}`, fakeJWT(t, f.sub), f.ttl)
	})
	return f
}

func TestPasswordSourceCaches(t *testing.T) {
	f := startAccount(t)
	now := time.Unix(1_700_000_000, 0)
	p := NewPasswordSource(f.srv.URL, "frontend", "", "agent-exp", "s3cret")
	p.Now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := p.Token(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.hit.Load(); got != 1 {
		t.Fatalf("%d token requests, want 1", got)
	}
	if len(f.forms) != 1 || f.forms[0] != "password frontend agent-exp" {
		t.Fatalf("form: %v", f.forms)
	}
	if len(f.secrets) != 1 || f.secrets[0] != "" {
		t.Fatalf("public client sent a secret: %v", f.secrets)
	}
}

// With token exchange the account logs in through the client that performs the exchange; that
// client is confidential and needs its secret.
func TestPasswordSourceSendsClientSecret(t *testing.T) {
	f := startAccount(t)
	p := NewPasswordSource(f.srv.URL, "agw-agent", "client-secret", "agent-exp", "s3cret")
	p.HTTP = f.srv.Client()
	if _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.forms) != 1 || f.forms[0] != "password agw-agent agent-exp" {
		t.Fatalf("form: %v", f.forms)
	}
	if len(f.secrets) != 1 || f.secrets[0] != "client-secret" {
		t.Fatalf("client secret not sent")
	}
}

func TestPasswordSourceRenewsBeforeExpiry(t *testing.T) {
	f := startAccount(t)
	now := time.Unix(1_700_000_000, 0)
	p := NewPasswordSource(f.srv.URL, "frontend", "", "agent-exp", "s3cret")
	p.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := p.Token(ctx); err != nil {
		t.Fatal(err)
	}
	// Still far from expiry: cached.
	now = now.Add(2 * time.Minute)
	if _, err := p.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.hit.Load(); got != 1 {
		t.Fatalf("%d requests after 2 min, want 1", got)
	}
	// Within refreshAt of exp: renewed.
	now = now.Add(4*time.Minute + 40*time.Second)
	if _, err := p.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.hit.Load(); got != 2 {
		t.Fatalf("%d requests before expiry, want 2", got)
	}
}

func TestPasswordSourceErrorHasNoSecret(t *testing.T) {
	f := startAccount(t)
	f.status = http.StatusUnauthorized
	p := NewPasswordSource(f.srv.URL, "frontend", "", "agent-exp", "s3cret")
	p.HTTP = f.srv.Client()
	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error text carries the password: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should name the status: %v", err)
	}
}

func TestPasswordSourceSubjectAndOwns(t *testing.T) {
	f := startAccount(t)
	f.sub = "sub-exp"
	f.ttl = 300
	p := NewPasswordSource(f.srv.URL, "frontend", "", "agent-exp", "s3cret")
	ctx := context.Background()
	sub, err := p.Subject(ctx)
	if err != nil || sub != "sub-exp" {
		t.Fatalf("subject: %q %v", sub, err)
	}
	if !p.Owns(ctx, "sub-exp") || !p.Owns(ctx, "agent-exp") || p.Owns(ctx, "sub-anna") {
		t.Fatal("Owns does not match the account")
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PasswordSource keeps a platform access token for the experiment account (X1.2): a password grant,
// renewed before the token expires. Experiment runs need no browser session, so the 12-hour limit of
// Service does not apply; the token itself still has the realm's access-token lifespan.
//
// The client is configurable. With token exchange on (AGW_PLATFORM_TOKEN_EXCHANGE=true), Keycloak
// only exchanges a token whose issuer is the requesting client or in its audience; the public
// "frontend" token is refused with "Client is not within the token audience". The confidential
// client agw-agent (which performs the exchange) has direct access grants enabled, so the same
// account can log in through it when its secret is configured.
//
// Neither the password nor the client secret appears in a request log or an error text.
type PasswordSource struct {
	issuer       string
	clientID     string
	clientSecret string
	user         string
	password     string

	// HTTP and Now are replaceable for tests; nil means the defaults.
	HTTP *http.Client
	Now  func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	sub       string
	disc      *discovery
	discAt    time.Time
}

// NewPasswordSource prepares the login of the experiment account. Nothing is requested until the
// first Token or Subject call, so the orchestrator also starts while Keycloak is unreachable.
func NewPasswordSource(issuer, clientID, clientSecret, user, password string) *PasswordSource {
	return &PasswordSource{
		issuer:       strings.TrimRight(issuer, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		user:         user,
		password:     password,
	}
}

func (p *PasswordSource) http() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

func (p *PasswordSource) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Token returns a valid access token of the experiment account, renewing it before it expires.
func (p *PasswordSource) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.now().Add(refreshAt).Before(p.expiresAt) {
		return p.token, nil
	}
	d, err := p.discoveryLocked(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", p.clientID)
	if p.clientSecret != "" {
		form.Set("client_secret", p.clientSecret)
	}
	form.Set("username", p.user)
	form.Set("password", p.password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("experiment account: token request not buildable")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.http().Do(req)
	if err != nil {
		// The transport error never contains the form (and thus never the password).
		return "", fmt.Errorf("experiment account: Keycloak not reachable: %w", err)
	}
	defer resp.Body.Close()
	var t struct {
		Access      string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("experiment account: token response %d unreadable", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || t.Access == "" {
		return "", fmt.Errorf("experiment account: Keycloak responds %d: %s %s", resp.StatusCode, t.Error, t.Description)
	}
	ttl := time.Duration(t.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	p.token, p.expiresAt = t.Access, p.now().Add(ttl)
	return p.token, nil
}

// Subject returns the sub of the experiment account. It is read from the account's token once and
// then cached; the API stores it as the owner of experiment chats (X1.3).
func (p *PasswordSource) Subject(ctx context.Context) (string, error) {
	p.mu.Lock()
	sub := p.sub
	p.mu.Unlock()
	if sub != "" {
		return sub, nil
	}
	tok, err := p.Token(ctx)
	if err != nil {
		return "", err
	}
	sub, err = tokenSubject(tok)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.sub = sub
	p.mu.Unlock()
	return sub, nil
}

// Owns reports whether owner names the experiment account, either by its user name (what the API
// writes into the request context) or by its sub (what chats store as owner). Without a cached sub
// the first call asks Keycloak; a later chat of a normal user is not delayed beyond that.
func (p *PasswordSource) Owns(ctx context.Context, owner string) bool {
	if owner == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(owner), []byte(p.user)) == 1 {
		return true
	}
	p.mu.Lock()
	sub := p.sub
	p.mu.Unlock()
	if sub != "" {
		return owner == sub
	}
	sub, err := p.Subject(ctx)
	if err != nil {
		return false
	}
	return owner == sub
}

// discoveryLocked reads the token endpoint; the caller holds p.mu.
func (p *PasswordSource) discoveryLocked(ctx context.Context) (*discovery, error) {
	if p.disc != nil && p.now().Sub(p.discAt) < time.Hour {
		return p.disc, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, errors.New("experiment account: discovery request not buildable")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("experiment account: discovery not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("experiment account: discovery responds %d", resp.StatusCode)
	}
	var d discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, errors.New("experiment account: discovery unreadable")
	}
	if strings.TrimRight(d.Issuer, "/") != p.issuer {
		return nil, fmt.Errorf("experiment account: discovery names issuer %q, configured is %q", d.Issuer, p.issuer)
	}
	if d.Token == "" {
		return nil, errors.New("experiment account: discovery without token_endpoint")
	}
	p.disc, p.discAt = &d, p.now()
	return p.disc, nil
}

// tokenSubject reads sub from a JWT. The token came from the configured token endpoint over the
// issuer's transport (https in production), so the payload is not verified again here.
func tokenSubject(tok string) (string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", errors.New("experiment account: token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("experiment account: token payload unreadable")
	}
	var c struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.Sub == "" {
		return "", errors.New("experiment account: token without sub")
	}
	return c.Sub, nil
}

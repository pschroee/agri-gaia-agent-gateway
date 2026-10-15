// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrHub: the hub (or the way to it) failed. Every failure of the hub API is this error, so that a
// tool can answer "unavailable" instead of an error the agent would retry (G1.13).
var ErrHub = errors.New("jupyterhub")

// ErrNoHubUser: the hub does not know the chat's user, which happens when they never opened
// JupyterHub (the hub creates users at the first OAuth login, plan E2). The gateway has no
// admin:users scope on purpose, so it says so instead of creating the user.
var ErrNoHubUser = errors.New("the hub does not know this user")

// userRe is JupyterHub's rule for user names.
var userRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{0,62}$`)

// serverRe is what may stand in the URL as a server name. The gateway builds its server names
// itself (Server), so this only has to keep the URL safe when a name comes from somewhere else.
var serverRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Hub is the client of the JupyterHub API: start, stop, state. It holds a service token with the
// scopes servers and read:users:name (plan E2), never admin:users.
type Hub struct {
	base  string
	token string
	http  *http.Client
}

// NewHub returns the client; hc nil uses a client of its own.
func NewHub(base, token string, hc *http.Client) *Hub {
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		hc = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	}
	return &Hub{base: strings.TrimRight(base, "/"), token: token, http: hc}
}

// check refuses a user or server name that would change the meaning of the URL.
func check(user, server string) error {
	if !userRe.MatchString(user) {
		return fmt.Errorf("%w: user name %q is not valid", ErrHub, user)
	}
	if !serverRe.MatchString(server) {
		return fmt.Errorf("%w: server name %q is not valid", ErrHub, server)
	}
	return nil
}

// Start starts the server of the user. A server that is already running counts as started (the hub
// answers 400 "already running"), and a hub that does not know the user is ErrNoHubUser.
func (h *Hub) Start(ctx context.Context, user, server string) error {
	if err := check(user, server); err != nil {
		return err
	}
	body, code, err := h.do(ctx, http.MethodPost, "/users/"+url.PathEscape(user)+"/servers/"+url.PathEscape(server), []byte("{}"))
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusCreated || code == http.StatusAccepted:
		return nil
	case code == http.StatusBadRequest && strings.Contains(strings.ToLower(string(body)), "already running"):
		return nil
	case code == http.StatusNotFound:
		return ErrNoHubUser
	case code >= 200 && code < 300:
		return nil
	}
	return fmt.Errorf("%w: starting the server %s answers %d: %s", ErrHub, server, code, strings.TrimSpace(string(body)))
}

// Ready reports whether the server answers (the hub's user model carries the state of each server).
func (h *Hub) Ready(ctx context.Context, user, server string) (bool, error) {
	if err := check(user, server); err != nil {
		return false, err
	}
	body, code, err := h.do(ctx, http.MethodGet, "/users/"+url.PathEscape(user), nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound {
		return false, ErrNoHubUser
	}
	if code < 200 || code >= 300 {
		return false, fmt.Errorf("%w: reading the user answers %d", ErrHub, code)
	}
	var doc struct {
		Servers map[string]struct {
			Ready bool `json:"ready"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("%w: user model unreadable: %v", ErrHub, err)
	}
	return doc.Servers[server].Ready, nil
}

// ServerState names what the hub knows about a server: "not started" (the user model has no entry),
// "starting" (an entry that is not ready yet) or "running" (ready). ErrNoHubUser when the hub does
// not know the user at all.
func (h *Hub) ServerState(ctx context.Context, user, server string) (string, error) {
	if err := check(user, server); err != nil {
		return "", err
	}
	body, code, err := h.do(ctx, http.MethodGet, "/users/"+url.PathEscape(user), nil)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", ErrNoHubUser
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("%w: reading the user answers %d", ErrHub, code)
	}
	var doc struct {
		Servers map[string]struct {
			Ready bool `json:"ready"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("%w: user model unreadable: %v", ErrHub, err)
	}
	s, known := doc.Servers[server]
	switch {
	case !known:
		return "not started", nil
	case s.Ready:
		return "running", nil
	}
	return "starting", nil
}

// HasUser reports whether the hub knows the user (ok false: it does not; the agent then asks the
// user to open JupyterHub once).
func (h *Hub) HasUser(ctx context.Context, user string) (bool, error) {
	if !userRe.MatchString(user) {
		return false, fmt.Errorf("%w: user name %q is not valid", ErrHub, user)
	}
	_, code, err := h.do(ctx, http.MethodGet, "/users/"+url.PathEscape(user), nil)
	if err != nil {
		return false, err
	}
	switch {
	case code == http.StatusNotFound:
		return false, nil
	case code >= 200 && code < 300:
		return true, nil
	}
	return false, fmt.Errorf("%w: reading the user answers %d", ErrHub, code)
}

// Stop stops the server of the user. A server that is not there counts as stopped; the volume of
// the user stays (the files of earlier runs stay readable).
func (h *Hub) Stop(ctx context.Context, user, server string) error {
	if err := check(user, server); err != nil {
		return err
	}
	body, code, err := h.do(ctx, http.MethodDelete, "/users/"+url.PathEscape(user)+"/servers/"+url.PathEscape(server), nil)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusAccepted || code == http.StatusNoContent || code == http.StatusNotFound:
		return nil
	case code >= 200 && code < 300:
		return nil
	}
	return fmt.Errorf("%w: stopping the server %s answers %d: %s", ErrHub, server, code, strings.TrimSpace(string(body)))
}

// do sends one request to the hub and returns body and status.
func (h *Hub) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if h.base == "" {
		return nil, 0, fmt.Errorf("%w: no address configured", ErrHub)
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrHub, err)
	}
	req.Header.Set("Authorization", "token "+h.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrHub, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrHub, err)
	}
	return raw, resp.StatusCode, nil
}

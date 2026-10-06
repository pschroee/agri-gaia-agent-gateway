package platform

import (
	"context"
	"io"
	"net/http"
	"time"
)

// ProbeMaxAge: a probe of the platform API is reused this long, so that polling status views do not
// put load on the platform.
const ProbeMaxAge = 10 * time.Second

// probeTimeout bounds a single probe.
const probeTimeout = 5 * time.Second

// maxErrLen limits error texts handed to the UI.
const maxErrLen = 300

// ExchangeStatus is the outcome of the last token exchange of a chat (also a failed attempt to get the
// subject token, e.g. because the user's login expired).
type ExchangeStatus struct {
	ChatID string    `json:"chat_id"`
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
}

// Probe is the result of an unauthenticated request to the platform API. Any HTTP answer counts as
// reachable (the root of the API may well answer 404); only a transport error does not.
type Probe struct {
	Reachable  bool      `json:"reachable"`
	HTTPStatus int       `json:"http_status,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// Info describes how the binding is set up, without secrets.
type Info struct {
	APIURL string `json:"api_url"`
	// Login: "user" (each chat acts with its owner's login, oidc mode) or "account" (password grant of the
	// configured account).
	Login string `json:"login"`
	// Account: the configured account (login "account" only).
	Account       string `json:"account,omitempty"`
	ClientID      string `json:"client_id"`
	TokenExchange bool   `json:"token_exchange"`
}

func truncErr(err error) string {
	s := err.Error()
	if len(s) > maxErrLen {
		s = s[:maxErrLen] + "…"
	}
	return s
}

func (c *Client) noteExchange(chatID string, err error) {
	st := ExchangeStatus{ChatID: chatID, At: time.Now().UTC(), OK: err == nil}
	if err != nil {
		st.Error = truncErr(err)
	}
	c.mu.Lock()
	c.exchanged[chatID] = st
	c.mu.Unlock()
}

// Info returns the setup of the binding.
func (c *Client) Info() Info {
	in := Info{APIURL: c.base.String(), ClientID: c.cfg.ClientID, TokenExchange: c.cfg.Exchange, Login: "account"}
	if c.cfg.Subject != nil {
		in.Login = "user"
	} else {
		in.Account = c.cfg.User
	}
	return in
}

// LastExchange returns the most recent token exchange among the chats for which mine reports true.
// The record lives in memory: after a restart it is empty until the next platform call.
func (c *Client) LastExchange(mine func(chatID string) bool) (ExchangeStatus, bool) {
	c.mu.Lock()
	list := make([]ExchangeStatus, 0, len(c.exchanged))
	for _, st := range c.exchanged {
		list = append(list, st)
	}
	c.mu.Unlock()
	var best ExchangeStatus
	found := false
	for _, st := range list {
		if (!found || st.At.After(best.At)) && mine(st.ChatID) {
			best, found = st, true
		}
	}
	return best, found
}

// Probe checks whether the platform API answers, without a token (GET on the base URL). The result is
// cached for ProbeMaxAge.
func (c *Client) Probe(ctx context.Context) Probe {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if !c.probe.CheckedAt.IsZero() && time.Since(c.probe.CheckedAt) < ProbeMaxAge {
		return c.probe
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	defer cancel()
	start := time.Now()
	p := Probe{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+"/", nil)
	if err == nil {
		req.Header.Set("Accept", "application/json")
		var res *http.Response
		if res, err = c.cfg.HTTP.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
			res.Body.Close()
			p.Reachable, p.HTTPStatus = true, res.StatusCode
		}
	}
	if err != nil {
		p.Error = truncErr(err)
	}
	p.LatencyMS = time.Since(start).Milliseconds()
	p.CheckedAt = time.Now().UTC()
	c.probe = p
	return p
}

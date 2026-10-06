package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Successful and failed exchanges are recorded per chat; LastExchange picks the newest among the given chats.
func TestLastExchange(t *testing.T) {
	f := &fakeKeycloak{}
	c, _ := exchangeClient(t, f)
	ctx := context.Background()
	get := Request{Method: "GET", Path: "/datasets"}
	if _, ok := c.LastExchange(func(string) bool { return true }); ok {
		t.Fatal("no exchange yet, none expected")
	}
	for _, chat := range []string{"chat-a", "chat-b"} {
		if _, err := c.Do(ctx, chat, get); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	all := func(string) bool { return true }
	if st, ok := c.LastExchange(all); !ok || st.ChatID != "chat-b" || !st.OK || st.Error != "" || st.At.IsZero() {
		t.Fatalf("newest exchange: %+v %v", st, ok)
	}
	onlyA := func(id string) bool { return id == "chat-a" }
	if st, ok := c.LastExchange(onlyA); !ok || st.ChatID != "chat-a" {
		t.Fatalf("filtered to own chats: %+v %v", st, ok)
	}
	if _, ok := c.LastExchange(func(string) bool { return false }); ok {
		t.Fatal("foreign chats must not show")
	}
}

func TestLastExchangeFailure(t *testing.T) {
	f := &fakeKeycloak{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "agw-agent", ClientSecret: "secret", Exchange: true,
		Subject: func(context.Context, string) (string, error) { return "", errors.New("user's login expired") }})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := c.Do(context.Background(), "chat-x", Request{Method: "GET", Path: "/datasets"}); err == nil && res.Status != "error" {
		t.Fatalf("call without subject token expected to fail: %+v", res)
	}
	st, ok := c.LastExchange(func(string) bool { return true })
	if !ok || st.OK || !strings.Contains(st.Error, "login expired") {
		t.Fatalf("failed exchange: %+v %v", st, ok)
	}
	if in := c.Info(); in.Login != "user" || in.Account != "" || !in.TokenExchange || in.ClientID != "agw-agent" {
		t.Fatalf("info in user mode: %+v", in)
	}
}

func TestInfoAccount(t *testing.T) {
	c, err := New(Config{APIURL: "https://api.example.org/", TokenURL: "https://kc.example.org/t", User: "svc", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if in := c.Info(); in.Login != "account" || in.Account != "svc" || in.ClientID != "frontend" || in.TokenExchange || in.APIURL != "https://api.example.org" {
		t.Fatalf("info in account mode: %+v", in)
	}
}

// Any HTTP answer counts as reachable, without a token, and the result is cached.
func TestProbe(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("probe must not send a token")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Probe(context.Background())
	if !p.Reachable || p.HTTPStatus != 404 || p.Error != "" || p.CheckedAt.IsZero() {
		t.Fatalf("probe: %+v", p)
	}
	c.Probe(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("probe not cached: %d requests", hits.Load())
	}
	// Stale cache: probe again; a closed server is unreachable.
	srv.Close()
	c.probeMu.Lock()
	c.probe.CheckedAt = time.Now().Add(-ProbeMaxAge - time.Second)
	c.probeMu.Unlock()
	if p := c.Probe(context.Background()); p.Reachable || p.Error == "" || p.HTTPStatus != 0 {
		t.Fatalf("probe of a closed server: %+v", p)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestNeedsMinIO(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/datasets", false},
		{"GET", "/datasets/5", false},
		{"PATCH", "/datasets/5", false},
		{"POST", "/train/config", false},
		{"GET", "/train/containers/3/status", false},
		{"POST", "/datasets", true},
		{"DELETE", "/datasets/5", true},
		{"GET", "/train/containers/3/model", true},
		{"GET", "/integrated-services/files", true},
		{"PATCH", "/integrated-services/files", true},
	}
	for _, c := range cases {
		if got := NeedsMinIO(Request{Method: c.method, Path: c.path}); got != c.want {
			t.Errorf("NeedsMinIO(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestExchangeAudiencePerRoute(t *testing.T) {
	f := &fakeKeycloak{}
	c, _ := exchangeClient(t, f)
	ctx := context.Background()
	for _, r := range []Request{
		{Method: "GET", Path: "/datasets"},
		{Method: "DELETE", Path: "/datasets/5"},
		{Method: "GET", Path: "/datasets/5"},
	} {
		if res, err := c.Do(ctx, "chat-a", r); err != nil || res.HTTPStatus != 200 {
			t.Fatalf("%s %s: %+v %v", r.Method, r.Path, res, err)
		}
	}
	if !reflect.DeepEqual(f.exchanges, []string{"backend", "backend,minio"}) {
		t.Fatalf("exchanges: %v", f.exchanges)
	}
}

func TestExchangePerToolCall(t *testing.T) {
	type callKey struct{}
	f := &fakeKeycloak{}
	c, _ := exchangeClient(t, f)
	c.cfg.CallKey = func(ctx context.Context) string {
		s, _ := ctx.Value(callKey{}).(string)
		return s
	}
	get := Request{Method: "GET", Path: "/datasets"}
	for _, call := range []string{"call-1", "call-1", "call-2"} {
		ctx := context.WithValue(context.Background(), callKey{}, call)
		if res, err := c.Do(ctx, "chat-a", get); err != nil || res.HTTPStatus != 200 {
			t.Fatalf("%s: %+v %v", call, res, err)
		}
	}
	if len(f.exchanges) != 2 {
		t.Fatalf("one exchange per tool call: %v", f.exchanges)
	}
	c.Forget("chat-a")
	if len(c.chats) != 0 {
		t.Fatalf("Forget keeps entries: %d", len(c.chats))
	}
}

func TestExchangeDefaultMaxAge(t *testing.T) {
	c, err := New(Config{APIURL: "https://api.example", TokenURL: "https://kc.example/token", ClientID: "agw-agent", ClientSecret: "s", User: "u", Password: "p", Exchange: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.ChatTokenMaxAge != 5*time.Minute {
		t.Fatalf("default max age: %v", c.cfg.ChatTokenMaxAge)
	}
}

func TestExchangeRenewedAfterMaxAge(t *testing.T) {
	f := &fakeKeycloak{}
	c, _ := exchangeClient(t, f)
	ctx := context.Background()
	get := Request{Method: "GET", Path: "/datasets"}
	if _, err := c.Do(ctx, "chat-a", get); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for k, ct := range c.chats {
		ct.expires = time.Now().Add(-time.Second)
		c.chats[k] = ct
	}
	c.mu.Unlock()
	if res, err := c.Do(ctx, "chat-a", get); err != nil || res.HTTPStatus != 200 {
		t.Fatalf("after max age: %+v %v", res, err)
	}
	if len(f.exchanges) != 2 {
		t.Fatalf("exchange after max age: %v", f.exchanges)
	}
}

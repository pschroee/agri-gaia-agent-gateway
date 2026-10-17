// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/config"
)

// upstreamSeen records what the provider received.
type upstreamSeen struct {
	mu     sync.Mutex
	bodies [][]byte
	hits   int
}

func (u *upstreamSeen) last() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return nil
	}
	return u.bodies[len(u.bodies)-1]
}

func (u *upstreamSeen) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

// gateProxy starts the proxy in front of an upstream that answers with respond. The catalog has three
// models: m1 takes temperature and seed, m2 takes neither, m3 takes temperature only.
func gateProxy(t *testing.T, rec *fakeRecorder, respond string) (*httptest.Server, *upstreamSeen) {
	t.Helper()
	seen := &upstreamSeen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.bodies = append(seen.bodies, b)
		seen.hits++
		seen.mu.Unlock()
		w.WriteHeader(200)
		io.WriteString(w, respond)
	}))
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk")
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m1","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY","models":[
	  {"id":"m1","sampling":{"temperature":true,"seed":true}},
	  {"id":"m2"},
	  {"id":"m3","sampling":{"temperature":true,"seed":false}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	px := New(cat)
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	t.Cleanup(s.Close)
	return s, seen
}

func postModel(t *testing.T, srv *httptest.Server, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// X3.1: an experiment run accepts only the chat's model — a model of the catalog is not enough.
func TestExperimentRefusesOtherModel(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Model: "p/m1"}}
	srv, seen := gateProxy(t, rec, `{"id":"r","model":"m1","choices":[]}`)

	resp, body := postModel(t, srv, `{"model":"m2"}`)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "model not allowed in this experiment run: m2") {
		t.Fatalf("other model: %d %s", resp.StatusCode, body)
	}
	if seen.count() != 0 {
		t.Fatalf("a refused model reached the provider: %d", seen.count())
	}
	if resp, body := postModel(t, srv, `{"model":"m1"}`); resp.StatusCode != 200 {
		t.Fatalf("the run's own model: %d %s", resp.StatusCode, body)
	}
}

// A product chat may use every model of the catalog — nothing more.
func TestNormalChatAcceptsCatalogue(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv, _ := gateProxy(t, rec, `{"id":"r","model":"m2","choices":[]}`)

	if resp, body := postModel(t, srv, `{"model":"m2"}`); resp.StatusCode != 200 {
		t.Fatalf("catalog model: %d %s", resp.StatusCode, body)
	}
	resp, body := postModel(t, srv, `{"model":"m4"}`)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "model not allowed: m4") {
		t.Fatalf("model outside the catalog: %d %s", resp.StatusCode, body)
	}
}

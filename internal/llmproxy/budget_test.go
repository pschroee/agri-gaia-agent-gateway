// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/config"
)

// budgetRecorder is a Recorder whose attribution carries a model-cost budget (A-E1, D1.9).
type budgetRecorder struct {
	chat string
	err  error
	seen int
}

func (b *budgetRecorder) Attribute(ip string) Attribution {
	if ip == "" {
		return Attribution{}
	}
	return Attribution{ChatID: b.chat, SlotID: "p-1", MaxConcurrent: 4,
		Budget: func(string) error { b.seen++; return b.err }}
}
func (b *budgetRecorder) Record(Call)          {}
func (b *budgetRecorder) LimitHit(string, int) {}

// A model call is refused with 429 and never reaches the provider once the plan's budget is used up; under
// budget it goes through (A-E1, D1.9).
func TestBudgetRefusesWhenUsedUp(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk")
	cat, _ := config.ParseCatalog([]byte(`{"default":"p/m1","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY","models":[{"id":"m1"}]}]}`))
	rec := &budgetRecorder{chat: "chat-1"}
	px := New(cat)
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	t.Cleanup(s.Close)

	call := func() *http.Response {
		resp, err := http.Post(s.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := call(); resp.StatusCode != 200 {
		t.Fatalf("under budget: %d", resp.StatusCode)
	}
	rec.err = errors.New("plan budget for model calls used up (1.00 of 1.00 USD); the plan is paused")
	resp := call()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "plan budget") {
		t.Fatalf("over budget: %d %s", resp.StatusCode, body)
	}
	if hits != 1 {
		t.Fatalf("a refused call reached the provider: %d", hits)
	}
	if rec.seen < 2 {
		t.Fatalf("the budget was not asked (%d)", rec.seen)
	}
}

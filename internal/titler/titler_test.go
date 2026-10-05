package titler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/config"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"Merge pandas DataFrames":                     "Merge pandas DataFrames",
		"\n  „Größe des Datensatzes“.  \nsecond line": "Größe des Datensatzes",
		"Titel: **Schwanzbeißen erkennen**":           "Schwanzbeißen erkennen",
		"# Train  model!":                             "Train model",
		"   ":                                         "",
		strings.Repeat("a", 70):                       strings.Repeat("a", 60) + " …",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleRequestAndUsage(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":"„Merge data“"}}],"usage":{"prompt_tokens":112,"completion_tokens":14,"prompt_cache_hit_tokens":100}}`)
	}))
	defer srv.Close()
	t.Setenv("TITLE_TEST_KEY", "secret")
	cat := &config.Catalog{Providers: []config.Provider{{
		ID: "ds", Upstream: srv.URL, API: "openai-completions", APIKeyEnv: "TITLE_TEST_KEY",
		TitleRequest: json.RawMessage(`{"thinking":{"type":"disabled"}}`),
		Models:       []config.Model{{ID: "flash"}},
	}}}
	r, err := New(cat, "").Title(context.Background(), "ds/flash", "how do I merge two dataframes?")
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "Merge data" || r.Model != "ds/flash" || r.Status != 200 {
		t.Fatalf("result: %+v", r)
	}
	if r.Usage != (config.Usage{Input: 12, Output: 14, CacheRead: 100}) {
		t.Fatalf("usage: %+v", r.Usage)
	}
	if auth != "Bearer secret" || got["model"] != "flash" || got["stream"] != false {
		t.Fatalf("request: %v %v", auth, got)
	}
	if th, _ := got["thinking"].(map[string]any); th["type"] != "disabled" {
		t.Fatalf("title_request missing: %v", got)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 || !strings.Contains(msgs[1].(map[string]any)["content"].(string), "dataframes") {
		t.Fatalf("messages: %v", msgs)
	}

	// A dedicated title model takes precedence; an unknown API kind is rejected.
	cat.Providers = append(cat.Providers, config.Provider{ID: "an", API: "anthropic-messages", Models: []config.Model{{ID: "m"}}})
	if _, err := New(cat, "an/m").Title(context.Background(), "ds/flash", "x"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("anthropic: %v", err)
	}
}

func TestTitleHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"too many"}}`)
	}))
	defer srv.Close()
	cat := &config.Catalog{Providers: []config.Provider{{ID: "ds", Upstream: srv.URL, API: "openai-completions", Models: []config.Model{{ID: "flash"}}}}}
	r, err := New(cat, "").Title(context.Background(), "ds/flash", "x")
	if err == nil || r.Status != 429 || r.Title != "" {
		t.Fatalf("error case: %+v %v", r, err)
	}
}

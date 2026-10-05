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
		"Pandas DataFrames zusammenführen":             "Pandas DataFrames zusammenführen",
		"\n  „Größe des Datensatzes“.  \nzweite Zeile": "Größe des Datensatzes",
		"Titel: **Schwanzbeißen erkennen**":            "Schwanzbeißen erkennen",
		"# Modell  trainieren!":                        "Modell trainieren",
		"   ":                                          "",
		strings.Repeat("a", 70):                        strings.Repeat("a", 60) + " …",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, erwartet %q", in, got, want)
		}
	}
}

func TestTitleRequestAndUsage(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("Pfad %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":"„Daten zusammenführen“"}}],"usage":{"prompt_tokens":112,"completion_tokens":14,"prompt_cache_hit_tokens":100}}`)
	}))
	defer srv.Close()
	t.Setenv("TITLE_TEST_KEY", "geheim")
	cat := &config.Catalog{Providers: []config.Provider{{
		ID: "ds", Upstream: srv.URL, API: "openai-completions", APIKeyEnv: "TITLE_TEST_KEY",
		TitleRequest: json.RawMessage(`{"thinking":{"type":"disabled"}}`),
		Models:       []config.Model{{ID: "flash"}},
	}}}
	r, err := New(cat, "").Title(context.Background(), "ds/flash", "wie führe ich zwei dataframes zusammen?")
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "Daten zusammenführen" || r.Model != "ds/flash" || r.Status != 200 {
		t.Fatalf("Ergebnis: %+v", r)
	}
	if r.Usage != (config.Usage{Input: 12, Output: 14, CacheRead: 100}) {
		t.Fatalf("Nutzung: %+v", r.Usage)
	}
	if auth != "Bearer geheim" || got["model"] != "flash" || got["stream"] != false {
		t.Fatalf("Anfrage: %v %v", auth, got)
	}
	if th, _ := got["thinking"].(map[string]any); th["type"] != "disabled" {
		t.Fatalf("title_request fehlt: %v", got)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 || !strings.Contains(msgs[1].(map[string]any)["content"].(string), "dataframes") {
		t.Fatalf("Nachrichten: %v", msgs)
	}

	// Eigenes Titelmodell hat Vorrang; unbekannte API-Art wird abgelehnt.
	cat.Providers = append(cat.Providers, config.Provider{ID: "an", API: "anthropic-messages", Models: []config.Model{{ID: "m"}}})
	if _, err := New(cat, "an/m").Title(context.Background(), "ds/flash", "x"); err == nil || !strings.Contains(err.Error(), "nicht unterstützt") {
		t.Fatalf("anthropic: %v", err)
	}
}

func TestTitleHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"zu viele"}}`)
	}))
	defer srv.Close()
	cat := &config.Catalog{Providers: []config.Provider{{ID: "ds", Upstream: srv.URL, API: "openai-completions", Models: []config.Model{{ID: "flash"}}}}}
	r, err := New(cat, "").Title(context.Background(), "ds/flash", "x")
	if err == nil || r.Status != 429 || r.Title != "" {
		t.Fatalf("Fehlerfall: %+v %v", r, err)
	}
}

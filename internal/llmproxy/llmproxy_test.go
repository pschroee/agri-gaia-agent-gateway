package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/config"
)

func setup(t *testing.T) (*httptest.Server, *[]*http.Request, *[]string) {
	t.Helper()
	var reqs []*http.Request
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, r)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"ok\":true}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk-echt")
	cat := &config.Catalog{Default: "p/m1", Providers: []config.Provider{{ID: "p", Upstream: up.URL, API: "openai-completions", APIKeyEnv: "TEST_KEY", Models: []config.Model{{ID: "m1"}}}}}
	px := httptest.NewServer(New(cat))
	t.Cleanup(px.Close)
	return px, &reqs, &bodies
}

func TestForwardsWithRealKeyAndStreams(t *testing.T) {
	px, reqs, bodies := setup(t)
	req, _ := http.NewRequest("POST", px.URL+"/llm/p/chat/completions", strings.NewReader(`{"model":"m1","stream":true}`))
	req.Header.Set("Authorization", "Bearer agw-proxy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("Antwort: %d %s", resp.StatusCode, body)
	}
	if len(*reqs) != 1 {
		t.Fatalf("Upstream-Aufrufe: %d", len(*reqs))
	}
	r := (*reqs)[0]
	if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-echt" {
		t.Fatalf("Upstream sah Pfad %q, Auth %q", r.URL.Path, r.Header.Get("Authorization"))
	}
	if (*bodies)[0] != `{"model":"m1","stream":true}` {
		t.Fatalf("Body verändert: %s", (*bodies)[0])
	}
}

func TestRejectsUnknownModelAndProvider(t *testing.T) {
	px, reqs, _ := setup(t)
	resp, _ := http.Post(px.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"teuer"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unbekanntes Modell: %d", resp.StatusCode)
	}
	resp, _ = http.Post(px.URL+"/llm/x/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unbekannter Anbieter: %d", resp.StatusCode)
	}
	if len(*reqs) != 0 {
		t.Fatal("abgewiesene Anfrage erreichte den Upstream")
	}
}

func TestRejectsNonPost(t *testing.T) {
	px, _, _ := setup(t)
	resp, _ := http.Get(px.URL + "/llm/p/models")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

// K2: Go ordnet JSON-Schlüssel ohne Rücksicht auf Groß-/Kleinschreibung zu,
// der Anbieter nicht. Doppelte model-Schlüssel dürfen die Liste nicht umgehen.
func TestRejectsAmbiguousModelKeys(t *testing.T) {
	px, reqs, _ := setup(t)
	for _, body := range []string{
		`{"model":"teuer","Model":"m1"}`,
		`{"Model":"m1","model":"teuer"}`,
		`{"MODEL":"m1"}`,
		`{"model":"m1","model":"teuer"}`,
		`{"model":["m1"]}`,
		`kein json`,
	} {
		resp, _ := http.Post(px.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(body))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", body, resp.StatusCode)
		}
	}
	if len(*reqs) != 0 {
		t.Fatalf("mehrdeutige Anfrage erreichte den Upstream: %d", len(*reqs))
	}
}

// K2: Nur die Pfade der jeweiligen API sind erlaubt.
func TestRejectsForeignPaths(t *testing.T) {
	px, reqs, _ := setup(t)
	for _, p := range []string{"/llm/p/files", "/llm/p/v1/batches", "/llm/p/chat/completions/../../files", "/llm/p/"} {
		resp, _ := http.Post(px.URL+p, "application/json", strings.NewReader(`{"model":"m1"}`))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if len(*reqs) != 0 {
		t.Fatal("fremder Pfad erreichte den Upstream")
	}
}

func TestBodyTooLarge(t *testing.T) {
	px, _, _ := setup(t)
	big := `{"model":"m1","x":"` + strings.Repeat("a", maxBody) + `"}`
	resp, _ := http.Post(px.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(big))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("übergroß: %d", resp.StatusCode)
	}
}

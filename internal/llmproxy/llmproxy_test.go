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
	t.Setenv("TEST_KEY", "sk-real")
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
		t.Fatalf("response: %d %s", resp.StatusCode, body)
	}
	if len(*reqs) != 1 {
		t.Fatalf("upstream calls: %d", len(*reqs))
	}
	r := (*reqs)[0]
	if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-real" {
		t.Fatalf("upstream saw path %q, auth %q", r.URL.Path, r.Header.Get("Authorization"))
	}
	if (*bodies)[0] != `{"model":"m1","stream":true}` {
		t.Fatalf("body changed: %s", (*bodies)[0])
	}
}

func TestRejectsUnknownModelAndProvider(t *testing.T) {
	px, reqs, _ := setup(t)
	resp, _ := http.Post(px.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"pricey"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown model: %d", resp.StatusCode)
	}
	resp, _ = http.Post(px.URL+"/llm/x/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", resp.StatusCode)
	}
	if len(*reqs) != 0 {
		t.Fatal("refused request reached the upstream")
	}
}

func TestRejectsNonPost(t *testing.T) {
	px, _, _ := setup(t)
	resp, _ := http.Get(px.URL + "/llm/p/models")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

// K2: Go matches JSON keys case-insensitively,
// the provider does not. Duplicate model keys must not bypass the list.
func TestRejectsAmbiguousModelKeys(t *testing.T) {
	px, reqs, _ := setup(t)
	for _, body := range []string{
		`{"model":"pricey","Model":"m1"}`,
		`{"Model":"m1","model":"pricey"}`,
		`{"MODEL":"m1"}`,
		`{"model":"m1","model":"pricey"}`,
		`{"model":["m1"]}`,
		`not json`,
	} {
		resp, _ := http.Post(px.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(body))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", body, resp.StatusCode)
		}
	}
	if len(*reqs) != 0 {
		t.Fatalf("ambiguous request reached the upstream: %d", len(*reqs))
	}
}

// K2: only the paths of the respective API are allowed.
func TestRejectsForeignPaths(t *testing.T) {
	px, reqs, _ := setup(t)
	for _, p := range []string{"/llm/p/files", "/llm/p/v1/batches", "/llm/p/chat/completions/../../files", "/llm/p/"} {
		resp, _ := http.Post(px.URL+p, "application/json", strings.NewReader(`{"model":"m1"}`))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if len(*reqs) != 0 {
		t.Fatal("foreign path reached the upstream")
	}
}

func TestBodyTooLarge(t *testing.T) {
	px, _, _ := setup(t)
	big := `{"model":"m1","x":"` + strings.Repeat("a", maxBody) + `"}`
	resp, _ := http.Post(px.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(big))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", resp.StatusCode)
	}
}

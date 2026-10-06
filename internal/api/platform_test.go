package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agw/internal/platform"
)

func getPlatformStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/platform", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET /api/platform: %d %s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlatformStatusOff(t *testing.T) {
	out := getPlatformStatus(t, &Server{Token: testToken})
	if out["configured"] != false || len(out) != 1 {
		t.Fatalf("binding off: %v", out)
	}
}

func TestPlatformStatus(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(api.Close)
	c, err := platform.New(platform.Config{APIURL: api.URL, TokenURL: api.URL + "/token", User: "svc", Password: "secret-pw"})
	if err != nil {
		t.Fatal(err)
	}
	out := getPlatformStatus(t, &Server{Token: testToken, Platform: c})
	if out["configured"] != true || out["login"] != "account" || out["account"] != "svc" || out["client_id"] != "frontend" ||
		out["token_exchange"] != false || out["api_url"] != api.URL {
		t.Fatalf("setup: %v", out)
	}
	probe, _ := out["probe"].(map[string]any)
	if probe["reachable"] != true || probe["http_status"] != float64(404) {
		t.Fatalf("probe: %v", out["probe"])
	}
	if _, ok := out["last_exchange"]; ok {
		t.Fatalf("no exchange yet: %v", out)
	}
	for k, v := range out {
		if s, ok := v.(string); ok && s == "secret-pw" {
			t.Fatalf("password leaked in %s", k)
		}
	}
}

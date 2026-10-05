package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// fakePlatformRecorder records every request that reaches the platform.
type fakePlatformRecorder struct {
	mu  sync.Mutex
	got []string
}

func (f *fakePlatformRecorder) reached() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func withRecordingPlatform(t *testing.T, e *env) *fakePlatformRecorder {
	t.Helper()
	f := &fakePlatformRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		f.mu.Lock()
		f.got = append(f.got, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/datasets":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":77,"name":"new"}`)
		case r.URL.Path == "/openapi.json":
			fmt.Fprint(w, `{"paths":{"/datasets":{"get":{"summary":"Get Datasets"}}}}`)
		case r.Method == "POST" && r.URL.Path == "/train/config":
			w.Header().Set("Location", "/tasks/41")
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return f
}

// Outline 7.3.1: every forbidden call directly to the authorization service, without a language model.
// Expected: all forbidden ones refused, and none reaches the platform; all allowed ones go through.
func TestConformanceThroughManager(t *testing.T) {
	e := setup(t)
	f := withRecordingPlatform(t, e)
	ctx := context.Background()
	mk := func(raw string) (string, string) {
		c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(raw)})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range delegation.OwnInConformance {
			res, id, _ := strings.Cut(o, ":")
			if err := e.st.AddDelegationObject(ctx, c.ID, res, id); err != nil {
				t.Fatal(err)
			}
		}
		return c.ID, e.m.live[c.ID].slot.ID
	}
	chatID, slot := mk(delegation.ConformanceDelegation)
	var exp map[string]any
	_ = json.Unmarshal([]byte(delegation.ConformanceDelegation), &exp)
	exp["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	expRaw, _ := json.Marshal(exp)
	expiredID, expiredSlot := mk(string(expRaw))

	results := map[string]bool{}
	cases := delegation.ConformanceCases()
	byRules := map[string][2]string{}
	for _, c := range cases {
		before := len(f.reached())
		id, s := chatID, slot
		if c.Expired {
			id, s = expiredID, expiredSlot
		}
		if c.Rules != "" {
			pair, ok := byRules[c.Rules]
			if !ok {
				a, b := mk(c.Rules)
				pair = [2]string{a, b}
				byRules[c.Rules] = pair
			}
			id, s = pair[0], pair[1]
		}
		req, err := c.Build()
		allowed := false
		if err == nil {
			res, err := e.m.PlatformCall(ctx, id, s, "cli", req)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			allowed = res.Status == "ok"
		}
		reached := len(f.reached()) > before
		results[c.Name] = allowed
		if allowed != c.Allowed {
			t.Errorf("%s [%s]: allowed=%v, want %v", c.Name, c.Group, allowed, c.Allowed)
		}
		if !c.Allowed && reached {
			t.Errorf("%s: forbidden call reached the platform: %v", c.Name, f.reached()[before:])
		}
		// Exception: API description, own paths (/_agw) do not reach the platform.
		if c.Allowed && !reached && c.Tool != "rights" {
			t.Errorf("%s: allowed call did not reach the platform", c.Name)
		}
	}
	report := delegation.Report(results, cases)
	t.Logf("conformance check through the manager:\n%s", report)
	if p := os.Getenv("AGW_CONFORMANCE_REPORT"); p != "" {
		_ = os.WriteFile(p, []byte(report), 0o644)
	}
}

// Provenance rule end to end: a created dataset is "own", another one is not, and that also holds
// after idling and resuming; likewise the task of a created training.
func TestDelegationProvenance(t *testing.T) {
	e := setup(t)
	f := withRecordingPlatform(t, e)
	ctx := context.Background()
	raw := `{"rules":[{"action":"create","resource":"dataset"},{"action":"update","resource":"dataset","ids":["own"]},
	  {"action":"create","resource":"training"},{"action":"read","resource":"dataset","ids":["own"]},
	  {"action":"read","resource":"task","ids":["own"]}],"confirm":"none"}`
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	call := func(m, p, body string) platform.Result {
		r := platform.Request{Method: m, Path: p}
		if body != "" {
			r.Body = json.RawMessage(body)
		}
		res, err := e.m.PlatformCall(ctx, c.ID, slot, "mcp", r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := call("PATCH", "/datasets/77", `{}`); r.Status != "denied" {
		t.Fatalf("foreign before creating: %+v", r)
	}
	if r := call("POST", "/datasets", `{"name":"new"}`); r.Status != "ok" {
		t.Fatalf("create: %+v", r)
	}
	if r := call("PATCH", "/datasets/77", `{}`); r.Status != "ok" {
		t.Fatalf("own dataset: %+v", r)
	}
	if r := call("PATCH", "/datasets/78", `{}`); r.Status != "denied" {
		t.Fatalf("foreign dataset: %+v", r)
	}
	// Training only on a dataset the delegation may read: here the own one (Review 5, W1).
	if r := call("POST", "/train/config", `{"provider":"T","dataset_id":5}`); r.Status != "denied" {
		t.Fatalf("training on a foreign dataset: %+v", r)
	}
	if r := call("POST", "/train/config", `{"provider":"T","dataset_id":77}`); r.Status != "ok" {
		t.Fatalf("create training: %+v", r)
	}
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, c.ID, "continue"); err != nil { // resume
		t.Fatal(err)
	}
	slot = e.m.live[c.ID].slot.ID
	if r := call("GET", "/tasks/41", ""); r.Status != "ok" {
		t.Fatalf("own task after resuming: %+v", r)
	}
	if r := call("PATCH", "/datasets/077", `{}`); r.Status != "ok" {
		t.Fatalf("own dataset after resuming (ID 077): %+v", r)
	}
	rights := call("GET", platform.RightsPath, "")
	if !strings.Contains(rights.Body, "dataset 77") || !strings.Contains(rights.Body, "task 41") {
		t.Fatalf("rights without created objects: %s", rights.Body)
	}
	for _, g := range f.reached() {
		if strings.Contains(g, "/datasets/78") {
			t.Fatalf("foreign dataset reached the platform: %v", f.reached())
		}
	}
}

// Stage "no protection": enforce false lets it through but records the violation.
func TestDelegationAuditOnly(t *testing.T) {
	e := setup(t)
	f := withRecordingPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[],"enforce":false,"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "api", platform.Request{Method: "DELETE", Path: "/models/3"})
	if res.Status != "ok" || !strings.Contains(res.Violation, "delete model 3") || len(f.reached()) != 1 {
		t.Fatalf("log only: %+v %v", res, f.reached())
	}
	if _, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"fly","resource":"dataset"}]}`)}); err == nil {
		t.Fatal("invalid delegation expected to be refused on creation")
	}
}

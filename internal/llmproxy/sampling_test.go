// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// X3.6: for an experiment run the proxy sets temperature and seed where the catalog says the model takes
// them, and records exactly the values it sent.
func TestSamplingSetWhenSupported(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true, Temperature: 0.2, Seed: 42}}
	srv, seen := gateProxy(t, rec, `{"id":"r","model":"m1","choices":[]}`)

	resp, body := postModel(t, srv, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, body)
	}
	var sent map[string]any
	if err := json.Unmarshal(seen.last(), &sent); err != nil {
		t.Fatalf("forwarded body is not JSON: %v (%s)", err, seen.last())
	}
	if sent["temperature"] != 0.2 || sent["seed"] != float64(42) {
		t.Fatalf("forwarded sampling: %v", sent)
	}
	if len(rec.calls) != 1 || rec.calls[0].Temperature == nil || *rec.calls[0].Temperature != 0.2 ||
		rec.calls[0].Seed == nil || *rec.calls[0].Seed != 42 {
		t.Fatalf("recorded sampling: %+v", rec.calls)
	}
}

// A model the catalog does not describe as taking the fields gets neither, and the call records NULL — the
// report says "not set" instead of claiming a value.
func TestSamplingSkippedWhenNot(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true, Temperature: 0.2, Seed: 42}}
	srv, seen := gateProxy(t, rec, `{"id":"r","model":"m2","choices":[]}`)

	resp, body := postModel(t, srv, `{"model":"m2","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, body)
	}
	for _, field := range []string{`"temperature"`, `"seed"`} {
		if strings.Contains(string(seen.last()), field) {
			t.Fatalf("%s was sent to a model that does not take it: %s", field, seen.last())
		}
	}
	if len(rec.calls) != 1 || rec.calls[0].Temperature != nil || rec.calls[0].Seed != nil {
		t.Fatalf("recorded sampling: %+v", rec.calls)
	}
}

// A product chat stays untouched even where the model would take the fields.
func TestSamplingNotForProductChats(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv, seen := gateProxy(t, rec, `{"id":"r","model":"m1","choices":[]}`)

	if resp, body := postModel(t, srv, `{"model":"m1"}`); resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(seen.last()), `"temperature"`) || strings.Contains(string(seen.last()), `"seed"`) {
		t.Fatalf("product chat got sampling: %s", seen.last())
	}
}

// A field that appears twice on the top level is refused: the provider reads a different one than a lenient
// reader would, so the recorded value could be wrong (CLAUDE.md, code review K2).
func TestDuplicateKeyRefused(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true, Temperature: 0.2, Seed: 42}}
	srv, seen := gateProxy(t, rec, `{"id":"r","model":"m1","choices":[]}`)

	resp, body := postModel(t, srv, `{"model":"m1","temperature":0.1,"temperature":0.9}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "present more than once") {
		t.Fatalf("duplicate key: %d %s", resp.StatusCode, body)
	}
	if seen.count() != 0 {
		t.Fatalf("a refused request reached the provider: %d", seen.count())
	}
}

// X3.7: the model the provider reports (and its system fingerprint) is recorded with the call, for JSON and
// for streamed answers.
func TestReportedModelRecorded(t *testing.T) {
	for _, tc := range []struct {
		name, respond, reported string
	}{
		{"JSON", `{"id":"r","model":"m1-2026","system_fingerprint":"fp_1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`, "m1-2026 fp_1"},
		{"SSE", `data: {"id":"r","model":"m1-2026","system_fingerprint":"fp_1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n", "m1-2026 fp_1"},
		{"Anthropic", `data: {"type":"message_start","message":{"id":"r","model":"claude-test-1","usage":{"input_tokens":5}}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n", "claude-test-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{chat: "chat-1"}
			srv, _ := gateProxy(t, rec, tc.respond)
			if resp, body := postModel(t, srv, `{"model":"m1"}`); resp.StatusCode != 200 {
				t.Fatalf("call: %d %s", resp.StatusCode, body)
			}
			if len(rec.calls) != 1 || rec.calls[0].ModelReported != tc.reported {
				t.Fatalf("reported model: %+v", rec.calls)
			}
		})
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/config"
)

// farmProxy starts the proxy in front of an upstream that behaves like a model farm: it answers with the
// headers that name the serving instance. The catalog has one model with the tag group "gb10" and instance
// headers, and one without either.
func farmProxy(t *testing.T, rec *fakeRecorder, respond string) (*httptest.Server, *upstreamSeen) {
	t.Helper()
	seen := &upstreamSeen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.bodies = append(seen.bodies, b)
		seen.hits++
		seen.mu.Unlock()
		w.Header().Set("X-Farm-Api-Base", "http://node-a.example.internal:8000/v1")
		w.Header().Set("X-Farm-Model-Id", strings.Repeat("z", 400))
		w.WriteHeader(200)
		io.WriteString(w, respond)
	}))
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk")
	cat, err := config.ParseCatalog([]byte(`{"default":"p/tagged","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY","models":[
	  {"id":"tagged","request_tags":["gb10"],"instance_headers":["x-farm-api-base","X-Farm-Model-Id","x-farm-absent"],"sampling":{"temperature":false,"seed":true}},
	  {"id":"plain"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	px := New(cat)
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	t.Cleanup(s.Close)
	return s, seen
}

const farmReply = `{"id":"r","model":"tagged","choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":300,"completion_tokens_details":{"reasoning_tokens":250}}}`

// The proxy sets the model's tags in every request. An agent that sends its own tags, or calls the proxy
// directly, does not get past it: the value is overwritten, and what was sent is recorded.
func TestProxySetsTags(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv, seen := farmProxy(t, rec, farmReply)
	for _, body := range []string{
		`{"model":"tagged","messages":[]}`,
		`{"model":"tagged","tags":["gb20","evil"],"messages":[]}`,
		`{"tags":null,"model":"tagged"}`,
	} {
		if resp, out := postModel(t, srv, body); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, out)
		}
		var sent struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(seen.last(), &sent); err != nil || len(sent.Tags) != 1 || sent.Tags[0] != "gb10" {
			t.Fatalf("%s forwarded as %s (%v)", body, seen.last(), err)
		}
	}
	// A duplicated tags key is refused (the provider would read another one than the proxy).
	before := seen.count()
	resp, out := postModel(t, srv, `{"model":"tagged","tags":["a"],"tags":["b"]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "present more than once") || seen.count() != before {
		t.Fatalf("duplicate tags: %d %s (reached upstream: %v)", resp.StatusCode, out, seen.count() != before)
	}
	// A model without tags is forwarded byte for byte.
	if resp, out := postModel(t, srv, `{"model":"plain","tags":["mine"]}`); resp.StatusCode != 200 || string(seen.last()) != `{"model":"plain","tags":["mine"]}` {
		t.Fatalf("untagged model: %d %s forwarded %s", resp.StatusCode, out, seen.last())
	}
	for _, c := range rec.calls[:3] {
		tags, _ := c.RequestMeta["tags"].([]string)
		if len(tags) != 1 || tags[0] != "gb10" {
			t.Fatalf("recorded tags: %+v", c.RequestMeta)
		}
	}
}

// The instance behind a farm is recorded per call, from the response headers the catalog names.
func TestProxyRecordsInstanceHeaders(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv, _ := farmProxy(t, rec, farmReply)
	if resp, out := postModel(t, srv, `{"model":"tagged"}`); resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, out)
	}
	if resp, out := postModel(t, srv, `{"model":"plain"}`); resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, out)
	}
	h := rec.calls[0].UpstreamHeaders
	if h["x-farm-api-base"] != "http://node-a.example.internal:8000/v1" {
		t.Fatalf("headers: %v", h)
	}
	if v := h["x-farm-model-id"]; len(v) != 256 {
		t.Fatalf("a long value is cut at 256 characters: %d", len(v))
	}
	if _, ok := h["x-farm-absent"]; ok || len(h) != 2 {
		t.Fatalf("an absent header is left out: %v", h)
	}
	if rec.calls[1].UpstreamHeaders != nil {
		t.Fatalf("a model without instance_headers records none: %v", rec.calls[1].UpstreamHeaders)
	}
}

// request_meta records the shape of what was sent: thinking on or off, the output limit, the tags.
func TestProxyRequestMeta(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv, _ := farmProxy(t, rec, farmReply)
	for _, body := range []string{
		`{"model":"tagged","max_tokens":12288,"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":true}}`,
		`{"model":"tagged","max_completion_tokens":777,"chat_template_kwargs":{"enable_thinking":false}}`,
		`{"model":"plain","thinking":{"type":"enabled"}}`,
		`{"model":"plain","thinking":{"type":"disabled"}}`,
		`{"model":"plain"}`,
	} {
		if resp, out := postModel(t, srv, body); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, out)
		}
	}
	c := rec.calls
	if c[0].RequestMeta["thinking"] != "on" || c[0].RequestMeta["max_tokens"] != int64(12288) {
		t.Fatalf("qwen on: %v", c[0].RequestMeta)
	}
	if c[1].RequestMeta["thinking"] != "off" || c[1].RequestMeta["max_tokens"] != int64(777) {
		t.Fatalf("qwen off, max_completion_tokens: %v", c[1].RequestMeta)
	}
	if c[2].RequestMeta["thinking"] != "on" || c[3].RequestMeta["thinking"] != "off" {
		t.Fatalf("deepseek form: %v %v", c[2].RequestMeta, c[3].RequestMeta)
	}
	if _, has := c[4].RequestMeta["thinking"]; has || len(c[4].RequestMeta) != 0 {
		t.Fatalf("nothing sent, nothing recorded: %v", c[4].RequestMeta)
	}
	// Message content is never part of it.
	b, _ := json.Marshal(c[0].RequestMeta)
	if strings.Contains(string(b), "messages") {
		t.Fatal("meta carries messages")
	}
}

// Seed only: the catalog says the model takes the seed but not the temperature, so an experiment sets the
// seed and leaves the temperature to the model's own generation config.
func TestProxySeedWithoutTemperatureAndTags(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1", attr: Attribution{Experiment: true, Temperature: 0, Seed: 7}}
	srv, seen := farmProxy(t, rec, farmReply)
	if resp, out := postModel(t, srv, `{"model":"tagged","temperature":0.5}`); resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, out)
	}
	var sent map[string]any
	_ = json.Unmarshal(seen.last(), &sent)
	if sent["seed"] != float64(7) || sent["tags"] == nil || rec.calls[0].Temperature != nil {
		t.Fatalf("forwarded %v, recorded %+v", sent, rec.calls[0])
	}
	if sent["temperature"] != 0.5 {
		t.Fatalf("the proxy must not touch a temperature the model does not take: %v", sent)
	}
}

func TestSetFieldsKeepsOthersAndSortsNewOnes(t *testing.T) {
	out, err := setFields([]byte(`{"b":1,"seed":3,"c":[1,2]}`), map[string]json.RawMessage{
		"seed": json.RawMessage("9"), "tags": json.RawMessage(`["x"]`), "alpha": json.RawMessage("true")})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"b":1,"seed":9,"c":[1,2],"alpha":true,"tags":["x"]}` {
		t.Fatalf("got %s", out)
	}
	if out, err := setFields([]byte(`{"a":1}`), nil); err != nil || string(out) != `{"a":1}` {
		t.Fatalf("no fields: %s %v", out, err)
	}
}

// Issue #281: pi's summarisation request is marked in request_meta, an ordinary turn is not. Only the start of the
// first message is read; its text is never recorded.
func TestProxyMarksCompactionRequest(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv, _ := farmProxy(t, rec, farmReply)
	sys := "You are a context summarization assistant. Your task is to read a conversation."
	for _, body := range []string{
		`{"model":"plain","messages":[{"role":"system","content":"` + sys + `"},{"role":"user","content":"<conversation>secret</conversation>"}]}`,
		`{"model":"plain","messages":[{"role":"developer","content":[{"type":"text","text":"` + sys + `"}]},{"role":"user","content":"x"}]}`,
		`{"model":"plain","messages":[{"role":"system","content":"You are pi, a coding agent."},{"role":"user","content":"hello"}]}`,
		`{"model":"plain","messages":[{"role":"user","content":"` + sys + `"}]}`,
		`{"model":"plain"}`,
	} {
		if resp, out := postModel(t, srv, body); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, out)
		}
	}
	c := rec.calls
	if c[0].RequestMeta["purpose"] != PurposeCompaction || c[1].RequestMeta["purpose"] != PurposeCompaction {
		t.Fatalf("compaction requests: %v %v", c[0].RequestMeta, c[1].RequestMeta)
	}
	for i := 2; i < 5; i++ {
		if _, has := c[i].RequestMeta["purpose"]; has {
			t.Fatalf("call %d marked: %v", i, c[i].RequestMeta)
		}
	}
	b, _ := json.Marshal(c[0].RequestMeta)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "summarization") {
		t.Fatalf("meta carries message text: %s", b)
	}
}

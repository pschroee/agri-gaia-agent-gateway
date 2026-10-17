// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"agw/internal/assess"
	"agw/internal/config"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/toolset"
)

// byName indexes the entries of an overview.
func byName(cs Capabilities) map[string]Capability {
	out := map[string]Capability{}
	for _, c := range cs.Capabilities {
		out[c.Name] = c
	}
	return out
}

// The four entries of this package answer from configuration and state (G1.8, G1.11): vision follows the
// current model's input field, internet the chat's switch, compute the calls left, bindings the chat's
// binding. The names the features own are reported as missing in this version.
func TestCapabilitiesBuiltIn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.opt.Toolsets, _ = toolset.Parse("cli")
	e.m.opt.ComputeMaxPerChat = 3
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}

	over := e.m.Capabilities(ctx, c.ID)
	if len(over.Capabilities) != len(knownCapabilities) {
		t.Fatalf("entries: %+v", over.Capabilities)
	}
	got := byName(over)

	// vision: the test catalog says nothing about input, so the model reads text only.
	if v := got["vision"]; v.Available || !strings.Contains(v.Detail, "deepseek-flash") || !strings.Contains(v.Detail, "text only") || v.Without == "" {
		t.Errorf("vision: %+v", v)
	}
	// internet: off by default, requestable.
	if i := got["internet"]; i.Available || !strings.Contains(i.Detail, "off") || !strings.Contains(i.Detail, "request") {
		t.Errorf("internet off: %+v", i)
	}
	if err := e.st.SetInternet(ctx, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if i := byName(e.m.Capabilities(ctx, c.ID))["internet"]; !i.Available || !strings.Contains(i.Detail, "on in this chat") {
		t.Errorf("internet on: %+v", i)
	}
	// compute: the counter of the chat, not the limit alone.
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("compute calls: %d", n)
	}
	if cm := byName(e.m.Capabilities(ctx, c.ID))["compute"]; !cm.Available || !strings.Contains(cm.Detail, "3 of 3 compute calls left") {
		t.Errorf("compute before: %+v", cm)
	}
	for i := 0; i < 2; i++ {
		if ok, err := e.st.ReserveCompute(ctx, c.ID, 3); err != nil || !ok {
			t.Fatalf("reserve %d: %v %v", i, ok, err)
		}
	}
	if cm := byName(e.m.Capabilities(ctx, c.ID))["compute"]; !cm.Available || !strings.Contains(cm.Detail, "1 of 3 compute calls left") {
		t.Errorf("compute after two: %+v", cm)
	}
	// bindings: the combination the chat was created with.
	if b := got["bindings"]; !b.Available || !strings.Contains(b.Detail, "cli") {
		t.Errorf("bindings: %+v", b)
	}
	// edge (edge D-E2), notebook (own code C-E3) and annotation (V1) are registered and report that this
	// setup has no service for them; their own tests are in capability_edge_test.go, notebook_test.go and
	// capability_annotation_test.go.
	if f := got["edge"]; f.Available || !strings.Contains(f.Detail, "platform binding") || f.Without == "" {
		t.Errorf("edge: %+v", f)
	}
	if f := got["notebook"]; f.Available || !strings.Contains(f.Detail, "not configured on this instance") {
		t.Errorf("notebook: %+v", f)
	}
	if f := got["annotation"]; f.Available || !strings.Contains(f.Detail, "not part of this setup") {
		t.Errorf("annotation: %+v", f)
	}
	if !strings.Contains(over.Summary, "vision: no") || !strings.Contains(over.Summary, "compute: yes") {
		t.Errorf("summary: %q", over.Summary)
	}

	// vision follows the model: a catalog that names image input reports yes.
	img, err := config.ParseCatalog([]byte(`{"default":"deepseek/bild","providers":[{"id":"deepseek","models":[{"id":"bild","name":"DeepSeek Bild","input":["text","image"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.m.cat = img
	c2, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if v := byName(e.m.Capabilities(ctx, c2.ID))["vision"]; !v.Available || !strings.Contains(v.Detail, "DeepSeek Bild") || !strings.Contains(v.Detail, "reads images") {
		t.Errorf("vision with an image model: %+v", v)
	}
	// The exec path reads no catalog of its own (issue #163): agw-platform in the sandbox posts to the
	// socket, and the gateway answers from the same catalog object. The CLI binding therefore reports
	// the same answer as the overview above. PlatformCall insists on a configured binding even for a
	// virtual path (it checks m.opt.Platform first); the call below is answered by the gateway and
	// reaches the fake platform at no point.
	countPlatform(t, e)
	res, err := e.m.PlatformCall(ctx, c2.ID, e.m.live[c2.ID].slot.ID, "cli",
		platform.Request{Method: "GET", Path: platform.CapabilitiesPath})
	if err != nil || res.Status != "ok" {
		t.Fatalf("cli capabilities: %+v %v", res, err)
	}
	var over2 Capabilities
	if err := json.Unmarshal([]byte(res.Body), &over2); err != nil {
		t.Fatalf("cli body is not JSON: %q", res.Body)
	}
	if v := byName(over2)["vision"]; !v.Available || !strings.Contains(v.Detail, "reads images") || !strings.Contains(over2.Summary, "vision: yes") {
		t.Errorf("vision over the cli binding: %+v, %q", v, over2.Summary)
	}
}

// The overview and the image review read one catalog entry (issue #163): for the same model and the
// same dataset they answer the same. Both sides resolve the chat's model with config.Catalog.ChatModel,
// so the default of a chat without a model cannot make them disagree.
func TestVisionMatchesImageReview(t *testing.T) {
	cat, err := config.ParseCatalog([]byte(`{"default":"p/bild","providers":[{"id":"p","models":[
		{"id":"bild","input":["text","image"]},
		{"id":"text","input":["text"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := assess.VisionPattern("")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"p/bild": true, "p/text": false} {
		v := visionFor(cat, id)
		ok, why := assess.ReviewAllowed(cat, pattern, id, "mnist-mini")
		if v.Available != want || ok != want {
			t.Errorf("%s: vision available=%v, image review allowed=%v, want %v", id, v.Available, ok, want)
		}
		if want && why != "" {
			t.Errorf("%s: review gave a reason although it may look: %q", id, why)
		}
		if !want && (!strings.Contains(v.Detail, "reads text only") || !strings.Contains(why, "reads text only")) {
			t.Errorf("%s: the two answers disagree about text only: %q / %q", id, v.Detail, why)
		}
	}
	// A chat without a model works with the catalog's default in both paths.
	id := cat.ChatModel("")
	v := visionFor(cat, id)
	allowed, _ := assess.ReviewAllowed(cat, pattern, id, "mnist-mini")
	if !v.Available || !allowed {
		t.Errorf("default model: vision=%v, review=%v", v.Available, allowed)
	}
}

// An entry a feature registered appears in the overview, with its own state (G1.11).
func TestCapabilitiesRegistered(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// notebook is registered by the feature itself (own code C-E3), so the fake entry has to be restored,
	// not just deleted: otherwise the feature's registration would be gone for the rest of the package.
	prevFunc, hadFunc := capabilityFuncs["notebook"]
	prevSetup, hadSetup := capabilitySetups["notebook"]
	registerCapability("notebook", func(_ context.Context, _ *Manager, chatID string) Capability {
		return Capability{Available: true, Detail: "the notebook server of this instance is reachable", Without: "run the code in your own sandbox"}
	}, func(s Setup) Capability {
		return Capability{Available: true, Detail: "a notebook server is configured"}
	})
	t.Cleanup(func() {
		if hadFunc {
			capabilityFuncs["notebook"] = prevFunc
		} else {
			delete(capabilityFuncs, "notebook")
		}
		if hadSetup {
			capabilitySetups["notebook"] = prevSetup
		} else {
			delete(capabilitySetups, "notebook")
		}
	})
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	nb := byName(e.m.Capabilities(ctx, c.ID))["notebook"]
	if !nb.Available || !strings.Contains(nb.Detail, "reachable") || nb.Without != "run the code in your own sandbox" {
		t.Fatalf("notebook: %+v", nb)
	}
	// The static part feeds the system note: vision (from the catalog) and bindings, and here also the
	// feature's line; state such as internet and compute calls left never belongs into the note.
	joined := strings.Join(SetupLines(Setup{Catalog: e.cat, Variant: "cli"}), "\n")
	for _, want := range []string{"vision: ", "bindings: ", "notebook: a notebook server is configured"} {
		if !strings.Contains(joined, want) {
			t.Errorf("setup lines without %q: %s", want, joined)
		}
	}
	for _, bad := range []string{"internet", "compute"} {
		if strings.Contains(joined, bad) {
			t.Errorf("setup lines hold state (%s): %s", bad, joined)
		}
	}
	if got := SetupLines(Setup{Catalog: e.cat, Variant: "cli"}); len(got) != 3 {
		t.Errorf("lines: %v", got)
	}
	// Without the feature's static part only vision and bindings are named.
	delete(capabilitySetups, "notebook")
	if got := SetupLines(Setup{Catalog: e.cat, Variant: "cli"}); len(got) != 2 {
		t.Errorf("lines without the feature: %v", got)
	}
}

// A capability that reads platform state through the cache is worked out once in 60 seconds (G1.11).
func TestCapabilitiesCached(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var calls int64
	// edge is registered by its own feature (edge D-E2), so the fake has to be restored, not just
	// deleted: otherwise the feature's registration would be gone for the rest of the package.
	prevFunc, hadFunc := capabilityFuncs["edge"]
	registerCapability("edge", func(_ context.Context, m *Manager, chatID string) Capability {
		return m.cachedCapability(chatID, "edge", func() Capability {
			atomic.AddInt64(&calls, 1)
			return Capability{Available: true, Detail: "1 edge device online"}
		})
	})
	t.Cleanup(func() {
		if hadFunc {
			capabilityFuncs["edge"] = prevFunc
		} else {
			delete(capabilityFuncs, "edge")
		}
	})
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := byName(e.m.Capabilities(ctx, c.ID))["edge"]; !got.Available || got.Detail != "1 edge device online" {
			t.Fatalf("call %d: %+v", i, got)
		}
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("the cache did not hold: %d platform reads", n)
	}
	// Another chat has its own state.
	c2, _ := e.m.Create(ctx, NewChat{})
	_ = e.m.Capabilities(ctx, c2.ID)
	if n := atomic.LoadInt64(&calls); n != 2 {
		t.Fatalf("per chat: %d", n)
	}
}

// Every binding reads the overview through the same tool table and gets the same JSON (G1.9, G1.10); it
// needs no approval and reaches no platform.
func TestCapabilitiesAllBindings(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	tool, ok := platform.Lookup("capabilities")
	if !ok {
		t.Fatal("the tool capabilities is missing from the table")
	}
	req, err := tool.Build(nil)
	if err != nil || req.Method != "GET" || req.Path != platform.CapabilitiesPath {
		t.Fatalf("tool builds %+v %v", req, err)
	}
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	bodies := map[string]string{}
	for _, via := range []string{"cli", "mcp", "api"} {
		res, err := e.m.PlatformCall(ctx, c.ID, slot, via, req)
		if err != nil || res.Status != "ok" || res.HTTPStatus != 200 {
			t.Fatalf("%s: %+v %v", via, res, err)
		}
		if !json.Valid([]byte(res.Body)) {
			t.Fatalf("%s: the body is not JSON: %q", via, res.Body)
		}
		bodies[via] = res.Body
	}
	if bodies["cli"] != bodies["mcp"] || bodies["mcp"] != bodies["api"] {
		t.Fatalf("the bindings differ: %v", bodies)
	}
	// No approval, no compute call, no platform call.
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("the overview asked the user: %+v", aps)
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("the overview used a compute call: %d", n)
	}
	if n := withoutEdgeReads(got()); len(n) != 0 {
		t.Fatalf("the overview reached the platform: %v", n)
	}
}

// withoutEdgeReads drops the device read of the capability edge (G1.11, edge D-E2): an entry that
// reads platform state may do so, everything else of the overview may not.
func withoutEdgeReads(calls []string) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if c == "GET /edge-devices" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// TestCapabilitiesNoApproval: the overview is reading and asks nobody, also with a delegation that
// confirms writes.
func TestCapabilitiesNoApproval(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"read","resource":"dataset","ids":["*"]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "GET", Path: platform.CapabilitiesPath})
	if err != nil || res.Status != "ok" {
		t.Fatalf("with a delegation: %+v %v", res, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("asked the user: %+v", aps)
	}
	if n := withoutEdgeReads(got()); len(n) != 0 {
		t.Fatalf("reached the platform: %v", n)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// X0.8/XE1: the fixed blocks hold in level "none" too — blocked paths, field allowlists, calls the
// gateway cannot classify, the compute limit and ConfirmEach (the last is covered by the auto
// approval, which is the only difference it makes in an experiment chat).
func TestFixedBlocksHoldInProtectionNone(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ComputeMaxPerChat = 1
	withPlatform(t, e)
	ctx := context.Background()
	c := createExperiment(t, e, "none", NewChat{Delegation: json.RawMessage(`{"rules":[]}`)})
	slot := e.m.live[c.ID].slot.ID
	call := func(req platform.Request) platform.Result {
		t.Helper()
		res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(platform.Request{Method: http.MethodGet, Path: "/urls/1"}); res.Status != "error" || !strings.Contains(res.Message, "blocked") {
		t.Errorf("blocked path: %+v", res)
	}
	if res := call(platform.Request{Method: http.MethodPatch, Path: "/datasets/1", Body: []byte(`{"bucket_name":"other"}`)}); res.Status != "denied" || !strings.Contains(res.Message, "refused:") {
		t.Errorf("field rule: %+v", res)
	}
	if res, refused := effectRefusal(delegation.Effect("")); !refused || res.Status != "error" {
		t.Errorf("unclassifiable call: %+v", res)
	}
	if res := call(platform.Request{Method: http.MethodPost, Path: "/train/containers/5/run"}); res.Status != "ok" {
		t.Errorf("first compute call: %+v", res)
	}
	if res := call(platform.Request{Method: http.MethodPost, Path: "/train/containers/5/run"}); res.Status != "error" || !strings.Contains(res.Message, "compute limit reached") {
		t.Errorf("compute limit: %+v", res)
	}
}

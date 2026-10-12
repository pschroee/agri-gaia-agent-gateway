// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"testing"

	"agw/internal/platform"
)

func TestEffectOfActionAndValid(t *testing.T) {
	want := map[string]Effect{
		Read: EffectRead, Create: EffectWrite, Update: EffectWrite, Run: EffectCompute, Delete: EffectIrreversible,
		"unknown": "",
	}
	for action, eff := range want {
		if got := effectOfAction(action); got != eff {
			t.Errorf("effectOfAction(%q) = %q, want %q", action, got, eff)
		}
	}
	for _, e := range []Effect{EffectRead, EffectWrite, EffectCompute, EffectIrreversible} {
		if !e.Valid() {
			t.Errorf("%q should be valid", e)
		}
	}
	for _, e := range []Effect{"", "delete", "Read"} {
		if e.Valid() {
			t.Errorf("%q should not be valid", e)
		}
	}
}

func TestClassifyEffect(t *testing.T) {
	cases := []struct {
		method, path string
		body         json.RawMessage
		query        map[string]string
		want         Effect
	}{
		// read
		{"GET", "/openapi.json", nil, nil, EffectRead},
		{"GET", "/datasets", nil, nil, EffectRead},
		{"GET", "/datasets/keyword", nil, nil, EffectRead},
		{"GET", "/datasets/catalogue", nil, nil, EffectRead},
		{"GET", "/datasets/convert/formats", nil, nil, EffectRead},
		{"GET", "/datasets/5", nil, nil, EffectRead},
		{"GET", "/models", nil, nil, EffectRead},
		{"GET", "/models/keyword", nil, nil, EffectRead},
		{"GET", "/models/3", nil, nil, EffectRead},
		{"GET", "/models/3/download", nil, nil, EffectRead},
		{"GET", "/train/providers", nil, nil, EffectRead},
		{"GET", "/train/architectures/p1", nil, nil, EffectRead},
		{"GET", "/train/config/export", nil, nil, EffectRead},
		{"GET", "/train/config/p1/a1", nil, nil, EffectRead},
		{"GET", "/train/containers", nil, nil, EffectRead},
		{"GET", "/train/containers/7", nil, nil, EffectRead},
		{"GET", "/train/containers/7/status", nil, nil, EffectRead},
		{"GET", "/train/containers/7/logs", nil, nil, EffectRead},
		{"GET", "/train/containers/7/config", nil, nil, EffectRead},
		{"GET", "/tasks", nil, nil, EffectRead},
		{"GET", "/tasks/4", nil, nil, EffectRead},
		{"GET", "/edge-devices", nil, nil, EffectRead},
		{"GET", "/edge-devices/3/deployments", nil, nil, EffectRead},
		{"GET", "/container-images", nil, nil, EffectRead},
		{"GET", "/container-images/repo/name:tag", nil, nil, EffectRead},
		{"GET", "/licenses/", nil, nil, EffectRead},
		{"GET", platform.RightsPath, nil, nil, EffectRead},
		{"GET", "/unknown/thing", nil, nil, EffectRead},
		// write (writing GETs and the ordinary writes)
		{"POST", "/datasets", nil, nil, EffectWrite},
		{"POST", "/datasets/import", nil, nil, EffectWrite},
		{"PATCH", "/datasets/5", json.RawMessage(`{"name":"x"}`), nil, EffectWrite},
		{"POST", "/datasets/5/annotate", nil, nil, EffectWrite},
		{"GET", "/datasets/5/download", nil, nil, EffectWrite},
		{"POST", "/models", nil, nil, EffectWrite},
		{"PATCH", "/models/3", json.RawMessage(`{}`), nil, EffectWrite},
		{"PUT", "/train/config", json.RawMessage(`{"container_id":7,"dataset_id":12}`), nil, EffectWrite},
		{"GET", "/train/containers/7/model", nil, nil, EffectWrite},
		{"POST", "/train/containers/7/stop", nil, nil, EffectWrite},
		{"PATCH", "/train/containers/7", json.RawMessage(`{}`), nil, EffectWrite},
		{"PATCH", "/train/containers/7/score", json.RawMessage(`{"score":1}`), nil, EffectWrite},
		{"POST", "/edge-devices", nil, nil, EffectWrite},
		{"GET", "/edge-devices/3", nil, nil, EffectWrite},
		{"GET", "/licenses/", nil, map[string]string{"return_cached": "false"}, EffectWrite},
		{"POST", "/unknown/thing", nil, nil, EffectWrite},
		{"PUT", "/unknown/thing", nil, nil, EffectWrite},
		{"PATCH", "/unknown/thing", nil, nil, EffectWrite},
		// compute
		{"POST", "/train/config", json.RawMessage(`{"dataset_id":12}`), nil, EffectCompute},
		{"POST", "/train/containers/7/run", nil, nil, EffectCompute},
		// irreversible
		{"DELETE", "/datasets/5", nil, nil, EffectIrreversible},
		{"DELETE", "/models/3", nil, nil, EffectIrreversible},
		{"DELETE", "/train/containers/7", nil, nil, EffectIrreversible},
		{"DELETE", "/tasks/4", nil, nil, EffectIrreversible},
		{"DELETE", "/edge-devices/3", nil, nil, EffectIrreversible},
		{"DELETE", "/container-images/repo/name:tag", nil, nil, EffectIrreversible},
		{"PATCH", "/datasets/5/toggle-public", nil, nil, EffectIrreversible},
		{"PATCH", "/models/3/toggle-public", nil, nil, EffectIrreversible},
		{"DELETE", "/unknown/thing", nil, nil, EffectIrreversible},
	}
	for _, c := range cases {
		req := platform.Request{Method: c.method, Path: c.path, Body: c.body, Query: c.query}
		got := Classify(req)
		if got.Effect != c.want {
			t.Errorf("Classify(%s %s) effect = %q, want %q (%s)", c.method, c.path, got.Effect, c.want, got)
		}
	}
}

func TestEveryRouteHasEffect(t *testing.T) {
	for _, rt := range routes {
		eff := rt.effect
		if eff == "" {
			eff = effectOfAction(rt.action)
		}
		if !eff.Valid() {
			t.Errorf("route %s %s has no valid effect class", rt.method, rt.re)
		}
	}
}

func TestWritesIsTheFloor(t *testing.T) {
	saved := routes
	routes = append(append([]route{}, routes...), r("GET", `/licenses/?`, Read, API, 0))
	t.Cleanup(func() { routes = saved })

	with := Classify(platform.Request{Method: http.MethodGet, Path: "/licenses/", Query: map[string]string{"return_cached": "false"}})
	if with.Effect != EffectWrite {
		t.Errorf("licenses with query: effect = %q, want write", with.Effect)
	}
	without := Classify(platform.Request{Method: http.MethodGet, Path: "/licenses/"})
	if without.Effect != EffectRead {
		t.Errorf("licenses without query: effect = %q, want read", without.Effect)
	}
}

func TestConformanceEffects(t *testing.T) {
	want := map[string]Effect{
		"list datasets":                           EffectRead,
		"read API description":                    EffectRead,
		"create dataset":                          EffectWrite,
		"change own dataset":                      EffectWrite,
		"GET creates model from container 7":      EffectWrite,
		"GET starts license analysis":             EffectWrite,
		"GET download writes annotation to MinIO": EffectWrite,
		"GET registers edge device":               EffectWrite,
		"start container 7":                       EffectCompute,
		"training on own dataset (control)":       EffectCompute,
		"delete another user's dataset":           EffectIrreversible,
		"make another user's dataset public":      EffectIrreversible,
		"delete unknown path":                     EffectIrreversible,
	}
	// The cases themselves live in conformance.go; every name above must exist there.
	found := 0
	for _, c := range ConformanceCases() {
		eff, ok := want[c.Name]
		if !ok {
			continue
		}
		found++
		req, err := c.Build()
		if err != nil {
			t.Errorf("%s: build: %v", c.Name, err)
			continue
		}
		if got := Classify(req).Effect; got != eff {
			t.Errorf("%s: effect = %q, want %q", c.Name, got, eff)
		}
	}
	if found != len(want) {
		t.Errorf("found %d of %d named conformance cases", found, len(want))
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

func TestCallChanged(t *testing.T) {
	step := store.PlanStep{Method: "POST", Path: "/container-deployments",
		Query: json.RawMessage(`{"a":"1"}`),
		Body:  json.RawMessage(`{"name":"x","container_image_id":0,"edge_device_id":1}`),
		Form:  json.RawMessage(`{"k":["v"]}`)}
	same := platform.Request{Method: "POST", Path: "/container-deployments", Query: map[string]string{"a": "1"},
		Body: json.RawMessage(`{"edge_device_id":1,"container_image_id":0,"name":"x"}`), Form: map[string][]string{"k": {"v"}}}
	if CallChanged(step, same) {
		t.Error("another key order counts as changed")
	}
	cases := map[string]func(r *platform.Request){
		"path": func(r *platform.Request) { r.Path = "/container-deployments/9" },
		"image": func(r *platform.Request) {
			r.Body = json.RawMessage(`{"name":"x","container_image_id":87,"edge_device_id":1}`)
		},
		"query": func(r *platform.Request) { r.Query = map[string]string{"a": "2"} },
		"form":  func(r *platform.Request) { r.Form = map[string][]string{"k": {"w"}} },
	}
	for name, mod := range cases {
		r := same
		mod(&r)
		if !CallChanged(step, r) {
			t.Errorf("%s: change not seen", name)
		}
	}
	bare := store.PlanStep{Method: "PUT", Path: "/container-deployments/{{step1.id}}/deploy"}
	if CallChanged(bare, platform.Request{Method: "PUT", Path: "/container-deployments/{{step1.id}}/deploy"}) {
		t.Error("an empty call counts as changed")
	}
	if !CallChanged(bare, platform.Request{Method: "PUT", Path: "/container-deployments/9/deploy"}) {
		t.Error("a resolved placeholder is not seen")
	}
	broken := store.PlanStep{Path: "/x", Body: json.RawMessage(`{`)}
	if !CallChanged(broken, platform.Request{Path: "/x"}) {
		t.Error("an unreadable stored body counts as unchanged")
	}
}

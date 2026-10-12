// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agw/internal/platform"
)

// modelJSON is GET /models/5 of the platform with all fields update_model must carry over.
const modelJSON = `{"id":5,"name":"old","format":"onnx","owner":"agent-test","bucket_name":"b","input_name":"image",` +
	`"input_datatype":"float32","input_shape":[1,3,64,64],"input_semantics":"NCHW","output_name":"out",` +
	`"output_datatype":"float32","output_shape":[1,2],"output_labels":null}`

// metadataBackend answers the reads of update_model with the model and the PATCH with 204 No Content.
type metadataBackend struct {
	fakeBackend
	reqs []string
}

func (m *metadataBackend) PlatformCall(_ context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req.Method+" "+req.Path+"|"+string(req.Body))
	switch req.Method {
	case http.MethodGet:
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: modelJSON}, nil
	case http.MethodPatch:
		return platform.Result{Status: "ok", HTTPStatus: 204}, nil
	}
	return platform.Result{Status: "error", Message: "unexpected call " + req.Method}, nil
}

func TestUpdateModelViaCLI(t *testing.T) {
	b := &metadataBackend{fakeBackend: fakeBackend{chat: "chat-m"}}
	c := start(t, b)
	code, r := cliPlatform(t, c, "update-model", `{"model_id":"5","name":"mnist_efficientnet","output_shape":"[1, 3]"}`)
	if code != 200 || r.Status != "ok" || r.HTTPStatus != 200 {
		t.Fatalf("update-model: %d %+v", code, r)
	}

	if len(b.reqs) != 3 || !strings.HasPrefix(b.reqs[0], "GET /models/5|") ||
		!strings.HasPrefix(b.reqs[1], "PATCH /models/5|") || !strings.HasPrefix(b.reqs[2], "GET /models/5|") {
		t.Fatalf("calls to the backend: %v", b.reqs)
	}
	body := strings.SplitN(b.reqs[1], "|", 2)[1]
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("PATCH body %q: %v", body, err)
	}
	if len(m) != len(platform.ModelPatchFields) || m["name"] != "mnist_efficientnet" || m["format"] != "onnx" {
		t.Fatalf("PATCH body: %s", body)
	}
	if shape, _ := m["output_shape"].([]any); len(shape) != 2 {
		t.Fatalf("output_shape in PATCH body: %s", body)
	}
	if _, has := m["owner"]; has {
		t.Fatalf("PATCH body carries owner: %s", body)
	}

	n := 0
	for _, entry := range b.calls {
		if strings.HasPrefix(entry, "cli:platform:") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("log entries of the steps: %d, want 3: %v", n, b.calls)
	}
}

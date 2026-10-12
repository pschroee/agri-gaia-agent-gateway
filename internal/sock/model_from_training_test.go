// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"strings"
	"testing"

	"agw/internal/platform"
)

// modelFromTrainingBackend answers the reads of create_model_from_training; the training has the given
// status. Every other path answers 404.
type modelFromTrainingBackend struct {
	fakeBackend
	status string
	reqs   []string
}

const modelFromTrainingAnswer = `{"id":21,"name":"EfficientNet (Torchvision) [mnist-mini, None Acc.]","format":"onnx",` +
	`"input_shape":[1,3,64,64],"input_datatype":"float32","input_semantics":"image","output_shape":[1,2],"output_datatype":"float32"}`

func (m *modelFromTrainingBackend) PlatformCall(_ context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req.String())
	switch req.Method + " " + req.Path {
	case "GET /train/containers/7/status":
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"status":"` + m.status + `"}`}, nil
	case "GET /train/containers/7":
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":7,"dataset_id":3}`}, nil
	case "GET /datasets/3":
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":3}`}, nil
	case "GET /train/containers/7/model":
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: modelFromTrainingAnswer}, nil
	}
	return platform.Result{Status: "error", HTTPStatus: 404, Body: `{"detail":"not found"}`}, nil
}

func TestModelFromTrainingViaCLI(t *testing.T) {
	b := &modelFromTrainingBackend{fakeBackend: fakeBackend{chat: "chat-mt"}, status: "exited"}
	c := start(t, b)
	code, r := cliPlatform(t, c, "model-from-training", `{"train_container_id":7}`)
	if code != 200 || r.Status != "ok" || !strings.Contains(r.Body, `"tensor_metadata":true`) {
		t.Fatalf("take over: %d %+v", code, r)
	}
	want := []string{"GET /train/containers/7/status", "GET /train/containers/7", "GET /datasets/3", "GET /train/containers/7/model"}
	if len(b.reqs) != len(want) {
		t.Fatalf("to the backend: %v", b.reqs)
	}
	for i, w := range want {
		if b.reqs[i] != w {
			t.Errorf("request %d = %q, want %q", i, b.reqs[i], w)
		}
	}
	if len(b.calls) != 4 {
		t.Fatalf("log: %v", b.calls)
	}
}

func TestModelFromTrainingRefusedWhileRunning(t *testing.T) {
	b := &modelFromTrainingBackend{fakeBackend: fakeBackend{chat: "chat-mt"}, status: "running"}
	c := start(t, b)
	code, r := cliPlatform(t, c, "model-from-training", `{"train_container_id":7}`)
	if code != 200 || r.Status != "error" || !strings.Contains(r.Message, "not finished") {
		t.Fatalf("running: %d %+v", code, r)
	}
	if len(b.reqs) != 1 {
		t.Fatalf("only the status may be read: %v", b.reqs)
	}
	if len(b.calls) != 2 || !strings.HasPrefix(b.calls[1], "cli:platform:refused: training container 7") {
		t.Fatalf("log: %v", b.calls)
	}
}

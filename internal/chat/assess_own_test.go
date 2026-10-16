// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// uploadedModel is one report of the gateway's hook for an uploaded model (R2.6).
type uploadedModel struct {
	ChatID  string
	ModelID int64
	SHA256  string
}

// A model uploaded from a file of the sandbox is reported to the gateway's hook with the chat, the model
// the platform created and the SHA-256 of the bytes the gateway read (R2.6). Only then can the origin of
// such a model be recorded — the agent never states it (P2).
func TestModelUploadNotifiesGateway(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var mu sync.Mutex
	var got []uploadedModel
	platform.SetOnModelUploaded(func(_ context.Context, chatID string, modelID int64, sha string) {
		mu.Lock()
		got = append(got, uploadedModel{ChatID: chatID, ModelID: modelID, SHA256: sha})
		mu.Unlock()
	})
	t.Cleanup(func() { platform.SetOnModelUploaded(nil) })
	reports := func() []uploadedModel {
		mu.Lock()
		defer mu.Unlock()
		return append([]uploadedModel(nil), got...)
	}
	// The platform answers an upload with the model it created.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":7,"name":"mnist_cnn","format":"onnx"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	chat, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	// What the socket builds for upload_model: the form, the file path and the bytes it read for it.
	sha := strings.Repeat("ab", 32)
	req, err := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/models",
		Form:  map[string][]string{"name": {"mnist_cnn"}, "description": {"own code"}, "format": {"onnx"}},
		Files: []platform.File{{Field: "modelfile", Path: "/workspace/model.onnx"}}})
	if err != nil {
		t.Fatal(err)
	}
	req.Uploads = []platform.Upload{{Field: "modelfile", Name: "model.onnx", Data: []byte("onnx-own"), SHA256: sha}}
	events, cancel := e.m.Subscribe(chat.ID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		res, _ := e.m.PlatformCall(ctx, chat.ID, e.m.live[chat.ID].slot.ID, "cli", req)
		done <- res
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.Status != "ok" || res.HTTPStatus != http.StatusOK {
		t.Fatalf("the upload: %+v", res)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(reports()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	list := reports()
	if len(list) != 1 {
		t.Fatalf("%d reports of the upload, want 1", len(list))
	}
	if list[0].ChatID != chat.ID || list[0].ModelID != 7 || list[0].SHA256 != sha {
		t.Errorf("the report: %+v (chat %s)", list[0], chat.ID)
	}
	// A rejected upload never reaches the platform, so nothing is reported either.
	req2 := req
	req2.Uploads = []platform.Upload{{Field: "modelfile", Name: "model.onnx", Data: []byte("onnx-own"), SHA256: sha}}
	done = make(chan platform.Result, 1)
	go func() {
		res, _ := e.m.PlatformCall(ctx, chat.ID, e.m.live[chat.ID].slot.ID, "cli", req2)
		done <- res
	}()
	ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.Status != "rejected" {
		t.Fatalf("a rejected upload: %+v", res)
	}
	if n := len(reports()); n != 1 {
		t.Errorf("a rejected upload was reported (%d reports)", n)
	}
}

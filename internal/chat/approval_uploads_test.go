// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// manyUploads is a dataset upload of n files in five folders, with real hashes as the socket reads them.
func manyUploads(t *testing.T, n int) platform.Request {
	t.Helper()
	req := platform.Request{Method: "POST", Path: "/datasets",
		Form: map[string][]string{"name": {"up164"}, "description": {"x"}, "dataset_type": {"AgriImageDataResource"}}}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/workspace/up/f%d/img_%04d.png", i%5, i)
		data := fmt.Sprintf("image %d", i)
		req.Files = append(req.Files, platform.File{Field: "files", Path: p})
		req.Uploads = append(req.Uploads, platform.Upload{Field: "files", Name: fmt.Sprintf("img_%04d.png", i), Data: []byte(data), SHA256: upSum(data)})
	}
	return req
}

// capturePlatform is a fake platform that keeps the bytes of every multipart POST it receives.
func capturePlatform(t *testing.T, e *env) func() []string {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
		}
		w.Header().Set("Location", "/datasets/9")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// 500 files fit into one approval: the card gets the summary and the hash, the list of every file is stored apart.
func TestPlatformCallUploadApprovalCarriesManifest(t *testing.T) {
	e := setup(t)
	got := capturePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req := manyUploads(t, 500)
	want, err := platform.ManifestOf(req)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "mcp", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	var sum platform.UploadManifest
	if err := json.Unmarshal(ap.Uploads, &sum); err != nil {
		t.Fatalf("uploads field %s: %v", ap.Uploads, err)
	}
	if sum.Files != 500 || sum.SHA256 != want.SHA256 || len(sum.Entries) != 0 || len(sum.Folders) != 5 {
		t.Fatalf("summary on the approval: %+v", sum)
	}
	if !strings.Contains(ap.Preview, want.SHA256) || !strings.Contains(ap.Summary, "Manifest sha256 "+want.SHA256) {
		t.Errorf("the hash must be in the preview and the summary:\n%s\n%s", ap.Preview, ap.Summary)
	}
	raw, err := e.m.ApprovalUploadEntries(ctx, c.ID, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	var entries []platform.UploadEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) != 500 {
		t.Fatalf("entries: %d %v", len(entries), err)
	}
	if _, err := e.m.ApprovalUploadEntries(ctx, "00000000-0000-0000-0000-000000000000", ap.ID); err == nil {
		t.Error("the list is only served for the chat of the approval")
	}
	_, _ = e.m.Decide(ctx, ap.ID, true)
	if r := <-done; r.Status != "ok" || len(got()) != 1 {
		t.Fatalf("after the approval: %+v %d", r, len(got()))
	}
}

// The platform receives the approved bytes, all of them.
func TestPlatformCallUploadSendsApprovedBytes(t *testing.T) {
	e := setup(t)
	got := capturePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req := manyUploads(t, 12)
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "mcp", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	_, _ = e.m.Decide(ctx, ap.ID, true)
	if r := <-done; r.Status != "ok" {
		t.Fatalf("result: %+v", r)
	}
	body := got()
	if len(body) != 1 {
		t.Fatalf("%d POSTs", len(body))
	}
	for i := 0; i < 12; i++ {
		if !strings.Contains(body[0], fmt.Sprintf("image %d", i)) {
			t.Errorf("file %d is missing in the request to the platform", i)
		}
	}
}

// Bytes that change after the approval are never sent: the call is refused with files_changed.
func TestPlatformCallUploadRefusesChangedBytes(t *testing.T) {
	e := setup(t)
	got := capturePlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req := manyUploads(t, 12)
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "mcp", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	req.Uploads[3].Data[0] ^= 1 // the slice shares its bytes with the call that waits
	_, _ = e.m.Decide(ctx, ap.ID, true)
	r := <-done
	if r.Status != "error" || r.Reason != "files_changed" || len(got()) != 0 {
		t.Fatalf("changed bytes: %+v, %d requests reached the platform", r, len(got()))
	}
}

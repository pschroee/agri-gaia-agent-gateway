// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/toolset"
)

// A note about a search names the tools in the forms of the chat's bindings (#200): one form in a chat with the
// REST binding alone, all of them joined by "or" in a chat with the command line and MCP, because the note
// answers no call.
func TestSearchNoteForms(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, c := range []struct{ toolsets, want string }{
		{"api", "Read GET /_agw/search/{search_id}, then declare the next trial with POST /_agw/search/{search_id}/trials."},
		{"cli,mcp", "Read agw-platform search-status or mcp_platform_search_status, then declare the next trial with agw-platform search-next or mcp_platform_search_next."},
	} {
		ts, err := toolset.Parse(c.toolsets)
		if err != nil {
			t.Fatal(err)
		}
		opt := e.opt
		opt.Toolsets = ts
		m := NewManager(e.st, e.p, e.cat, e.blobs, artifacts.NewBroker(), opt)
		cv, err := m.Create(ctx, NewChat{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := e.st.CreateSearch(ctx, store.Search{ChatID: cv.ID, Title: "t", Path: store.SearchPathTemplate,
			Objective: json.RawMessage(`{"metric":"val_acc","mode":"max"}`), Space: json.RawMessage(testSpaceJSON),
			Budget: json.RawMessage(`{"max_runs":3}`), Base: json.RawMessage(testBase), Holdout: json.RawMessage(`{"available":true,"eval_dataset":{"id":9}}`)})
		if err != nil {
			t.Fatal(err)
		}
		ends := time.Now().Add(time.Hour)
		if s, _, err = e.st.DecideSearch(ctx, s.ID, store.SearchRunning, &ends); err != nil {
			t.Fatal(err)
		}
		// The note itself, not its delivery: in a chat with the REST binding alone the queued note is taken
		// into a turn at once, so the queue is empty by the time it is read.
		text := m.RenderNote(ctx, s.ChatID, "Read {{tool:search_status}}, then declare the next trial with {{tool:search_next}}.")
		if !strings.Contains(text, c.want) || strings.Contains(text, "{{tool:") {
			t.Errorf("%s: %s", c.toolsets, text)
		}
		_, _ = m.Suspend(ctx, cv.ID)
	}
}

// The answer to a declared trial carries markers (rendered where the call's binding is known), and rendering it
// leaves no marker.
func TestTrialAnswerMarkers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	res, _ := e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":0.01},"note":"first"}`))
	if res.Status != "ok" {
		t.Fatalf("declared: %+v", res)
	}
	for _, want := range []string{"{{tool:clone_training}}", "{{tool:start_training}}"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("answer lacks %s: %s", want, res.Body)
		}
	}
	got := platform.Render(res.Body, toolset.API, toolset.Of(toolset.API))
	for _, want := range []string{"POST /train/containers/{train_container_id}/clone", "POST /train/containers/{train_container_id}/run"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered answer lacks %s: %s", want, got)
		}
	}
	if strings.Contains(got, "{{tool:") || !strings.Contains(got, "as train_container_id") {
		t.Errorf("rendered answer: %s", got)
	}
}

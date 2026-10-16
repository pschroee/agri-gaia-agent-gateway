// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"agw/internal/store"
)

// The display images of a subagent's answer are saved the same way as the main agent's (issue #120, F16, DL3.8): the
// key is the answer's response ID and the roots are the same (the subagent's tools run in the same execution
// sandbox, E9), so the subagent view loads them like any other answer. The stored entries keep only clipped text,
// which is why the raw message of the newly arrived chunk is read.
func TestSubagentAnswerImagesCaptured(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "subagent images"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	a.mu.Lock()
	a.files["/workspace/plot.png"] = []byte(pngBytes)
	a.mu.Unlock()
	// a chunk of a subagent's session file: the answer shows a chart
	chunk := `{"type":"session_info","id":"i1","name":"worker: draw a chart"}` + "\n" +
		`{"type":"message","id":"m1","message":{"role":"assistant","responseId":"resp-9","content":[{"type":"text","text":"Done: ![Chart](plot.png)"}]}}` + "\n"
	e.m.captureSubagentImages(c.ID, a, chunk)
	saved := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !saved; time.Sleep(10 * time.Millisecond) {
		_, err := e.st.GetChatImage(ctx, c.ID, "resp-9", "/workspace/plot.png")
		saved = err == nil
	}
	if !saved {
		t.Fatal("the image of the subagent's answer was not saved")
	}
	// the task of the subagent is a user message: nothing is saved from it, even when it names an image
	e.m.captureSubagentImages(c.ID, a, `{"type":"message","id":"u1","message":{"role":"user","responseId":"resp-1","content":[{"type":"text","text":"![x](plot.png)"}]}}`+"\n")
	if _, err := e.st.GetChatImage(ctx, c.ID, "resp-1", "/workspace/plot.png"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("image of a task: %v", err)
	}
	// broken lines and messages of other kinds change nothing
	e.m.captureSubagentImages(c.ID, a, "broken\n{\"type\":\"model_change\",\"id\":\"a1\"}\n")
	if _, err := e.st.GetChatImage(ctx, c.ID, "resp-9", "/workspace/missing.png"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("nothing else was saved: %v", err)
	}
}

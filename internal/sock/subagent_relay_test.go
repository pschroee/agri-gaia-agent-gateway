// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agw/internal/store"
)

type relayBackend struct {
	*fakeBackend
	got []SubagentRelayResult
}

func (r *relayBackend) SubagentRelayResult(_ context.Context, chatID string, res SubagentRelayResult) error {
	if res.Ref != "agw-0123abcd" {
		return store.ErrNotFound
	}
	r.got = append(r.got, res)
	return nil
}

// The reply of the relay command arrives at pi's socket (where the extension runs); the execution sandbox, which
// the agent controls, has no such route. A reply for an unknown ref is a 404.
func TestSubagentRelayResultOnlyAtPiSocket(t *testing.T) {
	b := &relayBackend{fakeBackend: &fakeBackend{chat: "chat-7"}}
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))
	post := func(c *http.Client, body string) int {
		resp, err := c.Post("http://agw/subagent-relay/result", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(pi, `{"ref":"agw-0123abcd","ok":true}`); code != 200 {
		t.Fatalf("pi socket: %d", code)
	}
	if code := post(pi, `{"ref":"agw-ffffffff","ok":true}`); code != 404 {
		t.Fatalf("unknown ref: %d", code)
	}
	if code := post(exec, `{"ref":"agw-0123abcd","ok":true}`); code != 404 {
		t.Fatalf("execution sandbox must not relay results: %d", code)
	}
	if len(b.got) != 1 {
		t.Fatalf("results taken: %+v", b.got)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// effectBackend logs the effect class that travels with each call (CallerLogger).
type effectBackend struct {
	platformBackend
	logged []string
}

func (b *effectBackend) LogCallBy(ctx context.Context, slotID, chatID, via, op, detail, result string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logged = append(b.logged, via+"|"+detail+"|"+store.EffectFrom(ctx))
}

// The class of a platform call is the same whichever binding made it (issue #63).
func TestPlatformEffectSameAcrossBindings(t *testing.T) {
	b := &effectBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-e", decide: "approved"}}}
	c := start(t, b)
	cliPlatform(t, c, "start-training", `{"train_container_id":3}`)
	cliPlatform(t, c, "datasets", `{"limit":2}`)
	cliPlatform(t, c, "request", `{"method":"PATCH","path":"/datasets/5/toggle-public","body":{}}`)
	apiCall(t, c, "DELETE", "/platform-api/datasets/5", "application/json", `{"key":"-","csrftoken":"-","sessionid":"-"}`, nil)
	apiCall(t, c, "POST", "/platform-api/train/containers/3/run", "application/json", `{}`, nil)
	want := []string{
		"cli|POST /train/containers/3/run|compute",
		"cli|GET /datasets?limit=2|read",
		"cli|PATCH /datasets/5/toggle-public|irreversible",
		"cli|DELETE /datasets/5|irreversible",
		"cli|POST /train/containers/3/run|compute",
	}
	if got := strings.Join(b.logged, ";"); got != strings.Join(want, ";") {
		t.Fatalf("logged effects:\n got %s\nwant %s", got, strings.Join(want, ";"))
	}
}

// refuseBackend refuses every call before it reaches the platform, as the compute limit does.
type refuseBackend struct {
	platformBackend
}

func (r *refuseBackend) PlatformCall(_ context.Context, _, _, _ string, _ platform.Request) (platform.Result, error) {
	return platform.Result{Status: "error", Message: platform.RefusedPrefix + "compute limit reached: test"}, nil
}

func TestPlatformRefusalLoggedAsRefused(t *testing.T) {
	b := &refuseBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-f", decide: "approved"}}}
	c := start(t, b)
	code, r := cliPlatform(t, c, "start-training", `{"train_container_id":3}`)
	if code != 200 || r.Status != "error" || r.Message != "refused: compute limit reached: test" {
		t.Fatalf("agent sees: %d %+v", code, r)
	}
	joined := strings.Join(b.calls, ";")
	if !strings.Contains(joined, "cli:platform:refused: compute limit reached: test") {
		t.Fatalf("log without the refusal: %v", b.calls)
	}
}

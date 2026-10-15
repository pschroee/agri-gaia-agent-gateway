// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agw/internal/platform"
)

// The entry annotation of the overview (V1, G1.8): it is available when the service is wired and CVAT
// answers, and it names the number of usable functions. Without the service the entry says that this
// setup does not have it.
func TestCapabilityAnnotation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(e.m.Capabilities(ctx, c.ID))["annotation"]
	if got.Available || !strings.Contains(got.Detail, "not part of this setup") {
		t.Fatalf("without a service: %+v", got)
	}
	if !strings.Contains(got.Without, "Annotate") {
		t.Errorf("without-text: %q", got.Without)
	}

	// A second chat avoids the short-lived cache of the entry (G1.11).
	c2, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	d := &seamDoer{res: platform.Result{Status: "ok", HTTPStatus: 200,
		Body: `{"functions":[{"id":"a"},{"id":"b"}],"hidden":1,"steps":[]}`}}
	e.m.RegisterVirtual(platform.AnnotationPrefix, d)
	got = byName(e.m.Capabilities(ctx, c2.ID))["annotation"]
	if !got.Available || got.Detail != "CVAT reachable, 2 usable pre-annotation functions" {
		t.Fatalf("with a service: %+v", got)
	}
	if calls := d.got(); len(calls) != 1 || !strings.HasSuffix(calls[0], platform.AnnotationPrefix+"/functions") {
		t.Errorf("the entry did not list the functions: %v", calls)
	}
	// The entry is cached: a second look does not call again.
	_ = byName(e.m.Capabilities(ctx, c2.ID))["annotation"]
	if calls := d.got(); len(calls) != 1 {
		t.Errorf("the cached entry listed again: %v", calls)
	}

	// A service whose CVAT does not answer reports that instead.
	c3, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	d.res = platform.Result{Status: "error", Message: "CVAT answers HTTP 503"}
	got = byName(e.m.Capabilities(ctx, c3.ID))["annotation"]
	if got.Available || !strings.Contains(got.Detail, "not usable right now") {
		t.Fatalf("CVAT down: %+v", got)
	}
}

// Without the service the path answers unavailable instead of reaching the platform (G1.5, G1.13).
func TestAnnotationPathUnavailable(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli",
		platform.Request{Method: http.MethodGet, Path: platform.AnnotationPrefix + "/functions"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != platform.StatusUnavailable || !strings.Contains(res.Message, "annotation") {
		t.Fatalf("unavailable: %+v", res)
	}
	if n := got(); len(n) != 0 {
		t.Errorf("an unconfigured path reached the platform: %v", n)
	}
}

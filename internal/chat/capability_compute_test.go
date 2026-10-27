// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"
	"testing"

	"agw/internal/config"
	"agw/internal/store"
)

// #253: the system note names the platform's hardware from AGW_PLATFORM_GPU: expected, none, or not configured.
func TestSetupComputeGPULine(t *testing.T) {
	for _, c := range []struct{ gpu, want string }{
		{"expected", "the platform has a GPU (set by the operator)"},
		{"none", "no GPU, CPU only (4 vCPU)"},
		{"", "whether the platform has a GPU is not configured; give runtimes for both cases"},
	} {
		got := setupCompute(Setup{Env: config.Env{PlatformGPU: c.gpu}})
		if !strings.HasPrefix(got.Detail, "Platform trainings: ") || !strings.Contains(got.Detail, c.want) || got.PlatformGPU != c.gpu {
			t.Errorf("%q: %+v", c.gpu, got)
		}
		lines := strings.Join(SetupLines(Setup{Env: config.Env{PlatformGPU: c.gpu}}), "\n")
		if !strings.Contains(lines, "compute: Platform trainings: ") {
			t.Errorf("%q: not in the setup lines: %s", c.gpu, lines)
		}
	}
}

// #253: the live entry carries platform_gpu and, once the chat has an ended trial that knows its device, names it.
func TestCapabilityComputeLastRunDevice(t *testing.T) {
	e := setup(t)
	e.m.opt.PlatformGPU = "expected"
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	cm := byName(e.m.Capabilities(ctx, c.ID))["compute"]
	if cm.PlatformGPU != "expected" || strings.Contains(cm.Detail, "last training") {
		t.Fatalf("before any trial: %+v", cm)
	}
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, Path: store.SearchPathTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTrialFigures(ctx, tr.ID, "ok", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.FinishTrial(ctx, tr.ID, store.TrialDone, ""); err != nil {
		t.Fatal(err)
	}
	if cm := byName(e.m.Capabilities(ctx, c.ID))["compute"]; !strings.Contains(cm.Detail, "last training of this chat ran on the GPU") {
		t.Errorf("GPU run: %+v", cm)
	}
	tr2, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, Path: store.SearchPathTemplate})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.st.SetTrialFigures(ctx, tr2.ID, "ok", false)
	_, _ = e.st.FinishTrial(ctx, tr2.ID, store.TrialDone, "")
	if cm := byName(e.m.Capabilities(ctx, c.ID))["compute"]; !strings.Contains(cm.Detail, "ran on the CPU") {
		t.Errorf("CPU run: %+v", cm)
	}
}

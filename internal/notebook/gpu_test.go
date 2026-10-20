// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

func TestApprovalDataLimitsGPU(t *testing.T) {
	req := approvalReq(t, platform.RunBody{Entry: "a.py", TimeoutMinutes: 5},
		platform.Upload{Field: "file", Name: "a.py", Data: []byte("print(1)\n"), SHA256: "aa"})
	for _, gpu := range []bool{false, true} {
		n := New(Config{GPU: gpu}, nil, nil, nil)
		raw, err := n.ApprovalData(context.Background(), req, nil)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Limits map[string]any `json:"limits"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		v, ok := m.Limits["gpu"]
		if !ok || v != gpu {
			t.Errorf("limits.gpu with GPU=%v: %v (present %v) in %s", gpu, v, ok, raw)
		}
	}
}

func TestServerStatusGPU(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	n, _ := testService(t, st, &fakeDocker{}, newFakeHub())
	n.cfg.GPU = true
	res, err := n.serverStatus(context.Background(), chatID, "agent-test", n.Server(chatID))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Body), &m); err != nil {
		t.Fatal(err)
	}
	if m["gpu"] != true || m["cpus"] != float64(2) || m["memory_gb"] != float64(8) {
		t.Errorf("status: %s", res.Body)
	}
}

func TestNotebookStateNamesGPU(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	n, _ := testService(t, st, &fakeDocker{}, hub)
	hub.mu.Lock()
	hub.ready, hub.server = true, n.Server(chatID)
	hub.mu.Unlock()
	if _, detail := n.NotebookState(context.Background(), chatID); !strings.Contains(detail, "no GPU") {
		t.Errorf("without GPU: %q", detail)
	}
	n.cfg.GPU = true
	if _, detail := n.NotebookState(context.Background(), chatID); !strings.Contains(detail, "a GPU (shared)") {
		t.Errorf("with GPU: %q", detail)
	}
}

func TestRunScriptRecordsGPU(t *testing.T) {
	script := runScript("r20260101120000-abcd", "train.py", nil, 10)
	gpu := strings.Index(script, "nvidia-smi -L 2>/dev/null | head -n 1 > .agw/gpu")
	run := strings.Index(script, "timeout --signal=TERM")
	if gpu < 0 || run < 0 || gpu > run {
		t.Errorf("the GPU line must stand before the run:\n%s", script)
	}
}

func TestParseStatusGPU(t *testing.T) {
	cases := []struct {
		out  string
		want *string
	}{
		{"gpu\tGPU 0: Tesla V100-DGXS-32GB (UUID: GPU-1234)\n", ptr("Tesla V100-DGXS-32GB")},
		{"gpu\t\n", ptr("none")},
		{"started\t2026-01-01T10:00:00Z\n", nil},
		{"gpu\tGPU 0: Evil\x1b[31m name (UUID: GPU-1)\n", ptr("Evil\\u001b[31m name")},
		{"gpu\tGPU 0: " + strings.Repeat("x", 300) + " (UUID: GPU-1)\n", ptr(strings.Repeat("x", 120))},
	}
	for _, c := range cases {
		m, _, err := parseStatus(c.out)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.want == nil && m.GPU != nil:
			t.Errorf("%q: expected no GPU marker, got %q", c.out, *m.GPU)
		case c.want != nil && (m.GPU == nil || *m.GPU != *c.want):
			got := "<nil>"
			if m.GPU != nil {
				got = *m.GPU
			}
			t.Errorf("%q: got %q, expected %q", c.out, got, *c.want)
		}
	}
}

func ptr(s string) *string { return &s }

func TestRunInfoNoteGPUMismatch(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	n.cfg.GPU = true
	ctx := context.Background()
	runID := "r20260101120000-abcd"
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: runID, ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60}); err != nil {
		t.Fatal(err)
	}
	reply := func(out string) {
		docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
			if len(cmd) > 2 && cmd[2] == statusScript {
				return []byte(out), 0
			}
			return nil, 0
		}
	}
	reply("started\t2026-01-01T10:00:00Z\ngpu\t\n")
	info, err := n.Status(ctx, chatID, runID)
	if err != nil || info.GPU != "none" || !strings.Contains(info.Note, "saw none") {
		t.Errorf("mismatch: %+v %v", info, err)
	}
	reply("started\t2026-01-01T10:00:00Z\ngpu\tGPU 0: Tesla V100 (UUID: GPU-1)\n")
	if info, err = n.Status(ctx, chatID, runID); err != nil || info.GPU != "Tesla V100" || info.Note != "" {
		t.Errorf("with GPU: %+v %v", info, err)
	}
	reply("started\t2026-01-01T10:00:00Z\n") // an older run recorded nothing
	if info, err = n.Status(ctx, chatID, runID); err != nil || info.GPU != "" || info.Note != "" {
		t.Errorf("old run: %+v %v", info, err)
	}
}

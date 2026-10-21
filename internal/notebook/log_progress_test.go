// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
	"agw/internal/watch"
)

// Log collapses redrawn progress bars (issue #178), and the watcher asks for 100 lines.
func TestLogCollapsesProgress(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, newFakeHub())
	ctx := context.Background()
	runID := "r20260101120000-abcd"
	if err := st.CreateNotebookRun(ctx, store.NotebookRun{ID: runID, ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var asked []string
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		switch {
		case len(cmd) > 2 && cmd[2] == statusScript:
			return []byte("started\t2026-01-01T10:00:00Z\n"), 0
		case len(cmd) > 2 && cmd[2] == logScript:
			asked = append(asked, cmd[len(cmd)-1])
			return []byte("start\nEpoch 1:   0%|          | 0/17 [00:00<?, ?it/s]\rEpoch 1: 100%|##########| 17/17 [00:03<00:00,  4.57it/s]\nend\n"), 0
		}
		return nil, 0
	}
	out, err := n.Log(ctx, chatID, runID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\r") || strings.Count(out, "Epoch 1") != 1 || !strings.Contains(out, "17/17") {
		t.Errorf("log: %q", out)
	}
	p, err := n.WatchKind().Poll(ctx, watch.Job{ID: "j1", ChatID: chatID, Kind: KindName, Ref: runID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.LogTail, "\r") {
		t.Errorf("log tail: %q", p.LogTail)
	}
	if len(asked) != 2 || asked[0] != strconv.Itoa(50) || asked[1] != "100" {
		t.Errorf("asked for lines: %v", asked)
	}
}

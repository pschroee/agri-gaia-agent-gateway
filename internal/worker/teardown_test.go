// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"agw/internal/pool"
)

// Issue #55: every teardown logs its reason and the slot, also for a slot whose start failed
// before pi's container existed (container is empty then). No Docker: the worker has nothing to remove.
func TestTeardownLogsReason(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := &Factory{}
	f.Destroy(pool.WithReason(context.Background(), pool.ReasonStartFailed), &Worker{dir: filepath.Join(t.TempDir(), "p-0123456789ab")})
	f.Destroy(context.Background(), &Worker{dir: filepath.Join(t.TempDir(), "p-ba9876543210")})
	out := buf.String()
	for _, want := range []string{
		`msg="slot torn down" slot=p-0123456789ab container="" reason=start_failed`,
		`msg="slot torn down" slot=p-ba9876543210 container="" reason=unspecified`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"fmt"

	"agw/internal/plan"
	"agw/internal/platform"
)

// pinPlanFiles writes the SHA-256 and size of every upload of a plan into the proposal (issue #70), so the
// approval covers the content read now, not a hash the agent wrote. A proposal that does not parse
// is left as it is; the manager reports it.
func (h *handler) pinPlanFiles(ctx context.Context, req *platform.Request) error {
	refs, err := plan.FileRefs(req.Body)
	if err != nil {
		return nil
	}
	pins := map[int][]plan.FilePin{}
	for idx, files := range refs {
		rq := platform.Request{Files: files}
		if err := h.readFiles(ctx, &rq); err != nil {
			return fmt.Errorf("step %d: %w", idx, err)
		}
		for _, u := range rq.Uploads {
			pins[idx] = append(pins[idx], plan.FilePin{SHA256: u.SHA256, Bytes: int64(len(u.Data))})
		}
	}
	body, err := plan.SetFilePins(req.Body, pins)
	if err != nil {
		return err
	}
	req.Body = body
	return nil
}

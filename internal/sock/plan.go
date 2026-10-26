// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"

	"agw/internal/plan"
	"agw/internal/platform"
)

// pinPlanFiles writes the SHA-256 and size of every upload of a plan into the proposal (issue #70), so the
// approval covers the content read now, not a hash the agent wrote. A step whose files cannot be read gets
// the reason as file_error and does not stop the others (issue #218): the orchestrator reports it with the
// step, together with every other problem of the proposal. A proposal that does not parse is left as it is;
// the manager reports it.
func (h *handler) pinPlanFiles(ctx context.Context, req *platform.Request) error {
	refs, err := plan.FileRefs(req.Body)
	if err != nil {
		return nil
	}
	pins := map[int][]plan.FilePin{}
	fileErrs := map[int]string{}
	for idx, files := range refs {
		rq := platform.Request{Files: files}
		if err := h.readFiles(ctx, &rq); err != nil {
			fileErrs[idx] = err.Error()
			continue
		}
		for _, u := range rq.Uploads {
			pins[idx] = append(pins[idx], plan.FilePin{SHA256: u.SHA256, Bytes: int64(len(u.Data))})
		}
	}
	body, err := plan.SetFilePins(req.Body, pins, fileErrs)
	if err != nil {
		return err
	}
	req.Body = body
	return nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"strings"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// TaskIDs lists the platform tasks a call started, for the task widget (#173): the task of the answer's
// Location, then every "task:<n>" part of the watched reference (a build of a function carries its task
// in the body, a chained reference several parts). Each id once, in a stable order; empty when the call
// started no task.
func TaskIDs(access delegation.Access, req platform.Request, res platform.Result) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && digitsRe.MatchString(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if id, ok := taskIDFromLocation(res.Location); ok {
		add(id)
	}
	if _, ref, ok := For(access, req, res); ok {
		for _, part := range strings.Split(ref, "|") {
			if id, ok := strings.CutPrefix(part, "task:"); ok {
				add(id)
			}
		}
	}
	return out
}

// StartedTaskIDs lists the platform tasks a successful call started, for the provenance register: the
// tasks of TaskIDs, plus the task a platform_task reference names plainly (the build of an annotation
// function answers with the bare id). A failed call started none. The ids come from the platform's answer to the
// gateway's own call, never from what the agent claims (BM17 E4, E3-1).
func StartedTaskIDs(access delegation.Access, req platform.Request, res platform.Result) []string {
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return nil
	}
	out := TaskIDs(access, req, res)
	if kind, ref, ok := For(access, req, res); ok && kind == kindPlatformTask {
		if id := TaskIDOf(ref); digitsRe.MatchString(id) {
			for _, o := range out {
				if o == id {
					return out
				}
			}
			out = append(out, id)
		}
	}
	return out
}

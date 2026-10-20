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

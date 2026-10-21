// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"net/http"
	"regexp"
	"strings"
)

// The calls the agent may never make for pre-annotation (V1, E3). They live in their own file so that
// other features can register their own blockers next to them.
//
// /cvat/auth/login returns the user's CVAT key and session (key, sessionid are not redacted): the
// session is the browser session of the user in CVAT, and the gateway holds it itself (E2). The two
// raw annotations routes are refused instead of merely unused: POST /datasets/{id}/annotate mounts
// the dataset's bucket in the CVAT container with root rights and checks no owner, and
// POST /datasets/auto-annotation passes a file name into a shell in the backend. Both have a checked
// path under /_agw/annotation (annotation_task, annotation_function in V2).
var (
	rawAnnotateRe     = regexp.MustCompile(`^/datasets/[^/]+/annotate$`)
	rawAutoAnnotateRe = regexp.MustCompile(`^/datasets/auto-annotation$`)
)

func init() {
	registerBlocker(func(r Request) string {
		p := strings.ToLower(r.Path)
		switch {
		case p == "/cvat" || strings.HasPrefix(p, "/cvat/"):
			return "CVAT is reached only through the annotation_ tools"
		case r.Method == http.MethodPost && rawAnnotateRe.MatchString(p):
			return "use {{tool:annotation_task}}"
		case r.Method == http.MethodPost && rawAutoAnnotateRe.MatchString(p):
			return "use {{tool:annotation_function}}"
		}
		return ""
	})
}

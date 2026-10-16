// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"net/http"
	"regexp"
)

// AudienceMinIO is the audience the backend needs when it passes the user's token on to MinIO.
const AudienceMinIO = "minio"

// minioRoute is a call that the backend passes the user's token on to MinIO with (issue #71).
// Method "*" stands for every method.
type minioRoute struct {
	method string
	path   *regexp.Regexp
}

// minioRoutes lists the calls that reach MinIO with the caller's token (checked against the
// backend code, see issue #71). A missing route fails at MinIO, so the list errs on the side of
// too many routes; no route gets more rights than it needs otherwise.
var minioRoutes = []minioRoute{
	{http.MethodPost, regexp.MustCompile(`^/datasets/?$`)},
	{http.MethodPost, regexp.MustCompile(`^/datasets/import/?$`)},
	{http.MethodGet, regexp.MustCompile(`^/datasets/[^/]+/download/?$`)},
	{http.MethodPatch, regexp.MustCompile(`^/datasets/[^/]+/toggle-public/?$`)},
	{http.MethodDelete, regexp.MustCompile(`^/datasets/[^/]+/?$`)},
	{http.MethodPost, regexp.MustCompile(`^/datasets/auto-annotation/?$`)},
	// Creates the CVAT task of a dataset (V1). The backend reads the dataset's annotation file from
	// MinIO with the caller's token while doing so (services/cvat/cvat_api.py, _annotation_file_exists
	// asks even when the dataset has no such file); MinIO only accepts a token whose audience holds
	// "minio" (MINIO_IDENTITY_OPENID_CLIENT_ID in the platform's compose file). Without this route
	// that read fails, the backend answers 500 and deletes the task it has just created in CVAT.
	{http.MethodPost, regexp.MustCompile(`^/cvat/tasks/[^/]+/?$`)},
	{http.MethodPost, regexp.MustCompile(`^/models/?$`)},
	{http.MethodGet, regexp.MustCompile(`^/models/[^/]+/download/?$`)},
	{http.MethodPatch, regexp.MustCompile(`^/models/[^/]+/toggle-public/?$`)},
	{http.MethodDelete, regexp.MustCompile(`^/models/[^/]+/?$`)},
	{http.MethodGet, regexp.MustCompile(`^/train/containers/[^/]+/model/?$`)},
	{http.MethodPost, regexp.MustCompile(`^/train/containers/[^/]+/run/?$`)},
	{http.MethodGet, regexp.MustCompile(`^/minio-proxy/.+`)},
	{http.MethodPost, regexp.MustCompile(`^/triton/?$`)},
	{"*", regexp.MustCompile(`^/integrated-services(/.*)?$`)},
}

// NeedsMinIO reports whether the backend passes the caller's token on to MinIO for this call.
func NeedsMinIO(r Request) bool {
	for _, rt := range minioRoutes {
		if (rt.method == "*" || rt.method == r.Method) && rt.path.MatchString(r.Path) {
			return true
		}
	}
	return false
}

// callAudiences returns the audiences for one call: minio only where NeedsMinIO holds, otherwise
// the configured audiences without minio. If minio is the only configured audience, it stays.
// The audience parameter of the exchange filters the audiences and adds none; scope would add.
func (c *Client) callAudiences(r Request) []string {
	if NeedsMinIO(r) {
		return c.cfg.Audiences
	}
	var auds []string
	for _, a := range c.cfg.Audiences {
		if a != AudienceMinIO {
			auds = append(auds, a)
		}
	}
	if len(auds) == 0 {
		return c.cfg.Audiences
	}
	return auds
}

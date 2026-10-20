// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"

	"agw/internal/platform"
	"agw/internal/store"
)

// StepManifest is the manifest of a step's uploads, computed from the stored files; ok is false for a step
// without uploads or a row stored before sizes were pinned (some Bytes nil).
func StepManifest(st store.PlanStep) (platform.UploadManifest, bool) {
	if len(st.Files) == 0 {
		return platform.UploadManifest{}, false
	}
	entries := make([]platform.UploadEntry, len(st.Files))
	for i, f := range st.Files {
		if f.Bytes == nil {
			return platform.UploadManifest{}, false
		}
		entries[i] = platform.UploadEntry{Field: f.Field, Path: f.Path, Bytes: *f.Bytes, SHA256: f.SHA256}
	}
	annotationLast := false
	if len(st.Form) > 0 {
		var form map[string][]string
		if json.Unmarshal(st.Form, &form) == nil && len(form["includes_annotation_file"]) > 0 {
			annotationLast = form["includes_annotation_file"][0] == "true"
		}
	}
	m, err := platform.ManifestOfEntries(entries, annotationLast)
	if err != nil {
		return platform.UploadManifest{}, false
	}
	return m, true
}

// WithUploads sets Uploads (the manifest without the list of every file) on every step that has a manifest and
// clears Files on it. For the API and the plan event only: the list of every file is fetched on demand.
// The steps are copied; the stored plan is not touched.
func WithUploads(p store.Plan) store.Plan {
	steps := make([]store.PlanStep, len(p.Steps))
	copy(steps, p.Steps)
	for i := range steps {
		m, ok := StepManifest(steps[i])
		if !ok {
			continue
		}
		if raw, err := json.Marshal(m.Summary()); err == nil {
			steps[i].Uploads = raw
			steps[i].Files = nil
		}
	}
	p.Steps = steps
	return p
}

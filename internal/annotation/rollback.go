// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"agw/internal/platform"
	"agw/internal/store"
)

// The rollback of the automatic annotations of a run or a fill (V3b, E21). CVAT deletes exactly the
// objects whose id it is given, independent of what wrote them (apps/engine/views.py:1060-1068,
// apps/dataset_manager/task.py:327-356: filter(pk__in=…) per kind). The gateway therefore deletes only
// what stands in its own register for this chat and this source, and only as long as it is unchanged:
// right before the delete it reads the task again and compares the fingerprint of every object. A
// correction the user made in CVAT stays, and the answer says how many.

const (
	// rollbackBatch is how many ids one delete call carries; a task with thousands of automatic
	// annotations costs one call per 200 of them.
	rollbackBatch = 200
	// ScopeAll and ScopeSuspicious are the two extents of a rollback: everything the source wrote, or
	// only what stands on the images the last verification named as suspicious.
	ScopeAll        = "all"
	ScopeSuspicious = "suspicious"
)

// rollbackAnswer is the answer of annotation_rollback.
type rollbackAnswer struct {
	stepList
	SourceID      string `json:"source_id"`
	Scope         string `json:"scope"`
	Deleted       int    `json:"deleted"`
	ChangedKept   int    `json:"changed_kept"`
	Missing       int    `json:"missing"`
	FramesEmptied int    `json:"frames_emptied"`
	Note          string `json:"note,omitempty"`
}

// rollback answers POST /_agw/annotation/rollbacks (V3b): it takes back the annotations the named run or
// fill wrote. What the user annotated by hand is never touched, and neither is an automatic annotation
// that was corrected in CVAT since.
func (s *Service) rollback(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	var body struct {
		SourceID string `json:"source_id"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failed(errors.New("the body of the rollback is not readable"))
	}
	if body.SourceID == "" {
		return failed(errors.New("source_id missing; the rollback names the run (annotation-run) or the fill (annotation-job) it takes back"))
	}
	if body.Scope == "" {
		body.Scope = ScopeAll
	}
	if body.Scope != ScopeAll && body.Scope != ScopeSuspicious {
		return failed(fmt.Errorf("scope must be %s or %s", ScopeAll, ScopeSuspicious))
	}
	// The register of this chat is the boundary (E8, E21): what another chat or another user wrote is
	// not reachable from here, whatever the agent claims.
	registered, err := s.St.AnnotationObjects(ctx, chatID, body.SourceID)
	if err != nil {
		return platform.Result{}, err
	}
	if len(registered) == 0 {
		return failed(fmt.Errorf("no automatic annotation of %q is registered in this chat; only what a run or a fill wrote can be taken back", body.SourceID))
	}
	var wanted map[int]bool
	if body.Scope == ScopeSuspicious {
		if wanted, err = s.suspiciousFrames(ctx, chatID, body.SourceID); err != nil {
			return failed(err)
		}
	}
	taskID := registered[0].TaskID
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		return failed(err)
	}
	// Right before deleting, the task is read: what the user changed or deleted since is left alone.
	current := currentObjects(ann)
	out := rollbackAnswer{stepList: stepList{}, SourceID: body.SourceID, Scope: body.Scope}
	var items []deleteItem
	frames := map[int]bool{}
	for _, row := range registered {
		if wanted != nil && !wanted[row.Frame] {
			continue
		}
		now, ok := current[objectKey(row.Kind, row.CVATID)]
		if !ok {
			out.Missing++
			continue
		}
		if now.source != sourceAuto || now.fingerprint != row.Fingerprint {
			out.ChangedKept++
			continue
		}
		items = append(items, deleteItem{kind: row.Kind, cvatID: row.CVATID,
			ref: store.ObjectRef{TaskID: row.TaskID, Kind: row.Kind, CVATID: row.CVATID}})
		frames[row.Frame] = true
	}
	var done []store.ObjectRef
	for len(items) > 0 {
		n := rollbackBatch
		if n > len(items) {
			n = len(items)
		}
		part := AnnotationsDelete{}
		refs := make([]store.ObjectRef, 0, n)
		for _, item := range items[:n] {
			switch item.kind {
			case store.ObjectTag:
				part.Tags = append(part.Tags, item.cvatID)
			case store.ObjectShape:
				part.Shapes = append(part.Shapes, item.cvatID)
			case store.ObjectTrack:
				part.Tracks = append(part.Tracks, item.cvatID)
			}
			refs = append(refs, item.ref)
		}
		if err := s.C.DeleteAnnotations(ctx, ses, taskID, part); err != nil {
			r.cvat("PATCH /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations action=delete", err)
			// What went through is written down; the rest stands and a second call can take it back.
			if derr := s.St.DeleteAnnotationObjects(ctx, chatID, done); derr != nil {
				slog.Warn("annotation register not cleaned", "chat", chatID, "source", body.SourceID, "error", derr)
			}
			return failed(err)
		}
		r.cvat("PATCH /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations action=delete", nil)
		out.Deleted += n
		done = append(done, refs...)
		items = items[n:]
	}
	if err := s.St.DeleteAnnotationObjects(ctx, chatID, done); err != nil {
		slog.Warn("annotation register not cleaned", "chat", chatID, "source", body.SourceID, "error", err)
	}
	// How many images the rollback emptied: read once more and count the frames that carry nothing now.
	if out.Deleted > 0 {
		after, err := s.C.TaskAnnotations(ctx, ses, taskID)
		r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
		if err == nil {
			left := annotationsByFrame(after)
			for frame := range frames {
				if !left[frame] {
					out.FramesEmptied++
				}
			}
		}
	}
	out.Note = fmt.Sprintf("%d automatic annotation(s) taken back; %d changed by hand and left in place, %d no longer in CVAT. Nothing the user annotated himself was touched.",
		out.Deleted, out.ChangedKept, out.Missing)
	out.stepList = r.steps()
	return okAnswer(out)
}

// suspiciousFrames reads the frames the last verification of this source named (V3b): scope suspicious
// goes by what the gateway reported, not by a rule recomputed on other data.
func (s *Service) suspiciousFrames(ctx context.Context, chatID, sourceID string) (map[int]bool, error) {
	raw, err := s.St.LastVerifyResult(ctx, chatID, sourceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no verification of %q stands in this chat; run annotation-check mode=verify first, or take everything back with scope %s", sourceID, ScopeAll)
	}
	if err != nil {
		return nil, err
	}
	var a struct {
		Suspicious []SuspiciousImage `json:"suspicious"`
	}
	if json.Unmarshal([]byte(raw), &a) != nil {
		return nil, errors.New("the stored verification of this chat is not readable")
	}
	out := map[int]bool{}
	for _, item := range a.Suspicious {
		out[item.Frame] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the verification of %q named no suspicious image; there is nothing to take back with scope %s", sourceID, ScopeSuspicious)
	}
	return out, nil
}

// currentObject is one annotation of a task as the rollback reads it: what it stands with right now.
type currentObject struct {
	source      string
	fingerprint string
}

// objectKey names one annotation in the index of a task: its kind and the id CVAT gave it.
func objectKey(kind string, id int64) string {
	return kind + ":" + strconv.FormatInt(id, 10)
}

// currentObjects indexes the annotations of a task by kind and id, with the source and the fingerprint
// every object carries now (V3b).
func currentObjects(ann Annotations) map[string]currentObject {
	out := map[string]currentObject{}
	for _, t := range ann.Tags {
		out[objectKey(store.ObjectTag, t.ID)] = currentObject{source: t.Source, fingerprint: tagFingerprint(t)}
	}
	for _, sh := range ann.Shapes {
		out[objectKey(store.ObjectShape, sh.ID)] = currentObject{source: sh.Source,
			fingerprint: shapeFingerprint(sh.Type, sh.Frame, sh.LabelID, sh.Points, sh.Attributes)}
	}
	for _, tr := range ann.Tracks {
		fp, _, _ := trackFingerprint(tr)
		source := tr.Source
		if source == "" && len(tr.Shapes) > 0 {
			source = tr.Shapes[0].Source
		}
		out[objectKey(store.ObjectTrack, tr.ID)] = currentObject{source: source, fingerprint: fp}
	}
	return out
}

// deleteItem is one registered annotation a rollback takes back, in the order of the batches.
type deleteItem struct {
	kind   string
	cvatID int64
	ref    store.ObjectRef
}

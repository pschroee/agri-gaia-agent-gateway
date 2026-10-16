// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"

	"agw/internal/store"
)

// The register of the annotations a run or a fill wrote (V3b, E21). CVAT deletes what it is told to
// delete, independent of the source of an object (views.py:1060-1068), so the gateway itself keeps
// which objects are its own, and it keeps a fingerprint of each: a rollback takes back only a
// registered object that still stands in CVAT unchanged, never a correction of the user.
//
// The fingerprint goes over frame, class, kind, the points rounded to a tenth of a pixel and the
// attributes — what a user changes in CVAT when he corrects an annotation.

// fingerprint hashes the parts of one annotation; the parts are joined with a separator that cannot
// occur inside them.
func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// tagFingerprint is the fingerprint of a classification annotation.
func tagFingerprint(t Tag) string {
	return fingerprint("tag", strconv.FormatInt(t.LabelID, 10), strconv.Itoa(t.Frame), attributesOf(t.Attributes))
}

// shapeFingerprint is the fingerprint of one box annotation; a track is hashed over all of its shapes
// with the same parts.
func shapeFingerprint(typ string, frame int, labelID int64, points []float64, attrs []Attribute) string {
	return fingerprint(typ, strconv.FormatInt(labelID, 10), strconv.Itoa(frame), pointsOf(points), attributesOf(attrs))
}

// pointsOf writes a box the way the fingerprint reads it: a tenth of a pixel is the resolution of the
// CVAT UI, so a coordinate a user did not touch keeps the fingerprint.
func pointsOf(points []float64) string {
	var b strings.Builder
	for i, p := range points {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(roundCoord(p*10)/10, 'f', -1, 64))
	}
	return b.String()
}

// attributesOf writes the attribute values in the order CVAT answers them.
func attributesOf(attrs []Attribute) string {
	var b strings.Builder
	for i, a := range attrs {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(strconv.Itoa(a.SpecID))
		b.WriteByte('=')
		b.WriteString(a.Value)
	}
	return b.String()
}

// autoObjects lists the annotations a run or a fill wrote (source == "auto") as the register keeps them.
// only restricts it to a set of frames (nil: every frame). The source, the chat and the task are set by
// the caller; the ids are CVAT's.
func autoObjects(ann Annotations, only map[int]bool) []store.AnnotationObject {
	out := []store.AnnotationObject{}
	keep := func(frame int) bool { return only == nil || only[frame] }
	for _, t := range ann.Tags {
		if t.Source != sourceAuto || !keep(t.Frame) {
			continue
		}
		out = append(out, store.AnnotationObject{Kind: store.ObjectTag, CVATID: t.ID, Frame: t.Frame,
			LabelID: t.LabelID, Fingerprint: tagFingerprint(t)})
	}
	for _, sh := range ann.Shapes {
		if sh.Source != sourceAuto || sh.Outside || !keep(sh.Frame) {
			continue
		}
		out = append(out, store.AnnotationObject{Kind: store.ObjectShape, CVATID: sh.ID, Frame: sh.Frame,
			LabelID: sh.LabelID, Fingerprint: shapeFingerprint(sh.Type, sh.Frame, sh.LabelID, sh.Points, sh.Attributes)})
	}
	for _, tr := range ann.Tracks {
		fp, first, automatic := trackFingerprint(tr)
		if !automatic || !keep(first) {
			continue
		}
		out = append(out, store.AnnotationObject{Kind: store.ObjectTrack, CVATID: tr.ID, Frame: first,
			LabelID: tr.LabelID, Fingerprint: fp})
	}
	return out
}

// trackFingerprint hashes a track over its automatic shapes and names the frame of the first of them;
// automatic is false when the track carries no automatic shape at all. The rollback and the register
// ask it the same way, so that an unchanged track keeps its fingerprint.
func trackFingerprint(tr Track) (fp string, frame int, automatic bool) {
	parts := make([]string, 0, len(tr.Shapes)+3)
	parts = append(parts, "track", strconv.FormatInt(tr.LabelID, 10), attributesOf(tr.Attributes))
	first := -1
	for _, sh := range tr.Shapes {
		source := sh.Source
		if source == "" {
			source = tr.Source
		}
		if source != sourceAuto || sh.Outside {
			continue
		}
		if first < 0 {
			first = sh.Frame
		}
		parts = append(parts, shapeFingerprint(sh.Type, sh.Frame, tr.LabelID, sh.Points, sh.Attributes))
	}
	if first < 0 {
		return "", 0, false
	}
	return fingerprint(parts...), first, true
}

// snapshotRun records the automatic annotations of a task after a run reached finished (V3b). A run
// works on a task that carried no annotation (E12: existing=keep is refused on a task that has some,
// replace deletes them all first), so everything that stands with source "auto" when it ends is what
// the run wrote — and only this register lets the gateway take it back. It happens once, on the
// transition to finished; a failure is logged and leaves the run as it is.
func (s *Service) snapshotRun(ctx context.Context, r *recorder, ses session, chatID, sourceID string, taskID int64) {
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		slog.Warn("annotation register not written", "chat", chatID, "source", sourceID, "error", err)
		return
	}
	objs := autoObjects(ann, nil)
	for i := range objs {
		objs[i].SourceID, objs[i].ChatID, objs[i].TaskID = sourceID, chatID, taskID
	}
	if err := s.St.AddAnnotationObjects(ctx, objs); err != nil {
		slog.Warn("annotation register not written", "chat", chatID, "source", sourceID, "error", err)
	}
}

// registerAutomatic records the automatic annotations of the frames a fill just wrote (V3b). It reads
// the task right after the write and takes exactly the objects of source "auto" on those frames: what
// the answer of the write itself carries is not relied on. A failure is logged and the job runs on —
// the annotations stand in CVAT, only their register is missing.
func (s *Service) registerAutomatic(ctx context.Context, r *recorder, ses session, chatID, sourceID string, taskID int64, frames []int) {
	if len(frames) == 0 {
		return
	}
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		slog.Warn("annotation register not written", "chat", chatID, "source", sourceID, "error", err)
		return
	}
	only := map[int]bool{}
	for _, f := range frames {
		only[f] = true
	}
	objs := autoObjects(ann, only)
	for i := range objs {
		objs[i].SourceID, objs[i].ChatID, objs[i].TaskID = sourceID, chatID, taskID
	}
	if err := s.St.AddAnnotationObjects(ctx, objs); err != nil {
		slog.Warn("annotation register not written", "chat", chatID, "source", sourceID, "error", err)
	}
}

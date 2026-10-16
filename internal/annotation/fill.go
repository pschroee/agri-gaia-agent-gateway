// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"errors"
	"strconv"
)

// The fill of V3: the gateway asks the function about the images of a task that carry no annotation at
// all and writes what it answers into exactly those images. A run over the whole task would pile
// machine objects next to the manual ones (E12); a fill leaves every annotated image alone, and it
// checks again right before it writes, because the user may annotate in CVAT while the job runs.

// fillBatch is how many images one write to CVAT carries: the plan fixes 20 (V3). Writing image by
// image would be one CVAT call per image.
const fillBatch = 20

// fillAnswer is the result of a finished fill job.
type fillAnswer struct {
	// Filled is how many images the job wrote annotations on.
	Filled int `json:"filled"`
	// SkippedAnnotated is how many images it left alone, because they carry an annotation (one the user
	// made while the job ran counts here too).
	SkippedAnnotated int `json:"skipped_annotated"`
	// EmptyResults is how many images the function answered nothing on (or nothing usable).
	EmptyResults int `json:"empty_results"`
}

// frameAnnotations is what one image gets; the job collects them and writes them in batches.
type frameAnnotations struct {
	frame  int
	tags   []NewTag
	shapes []NewShape
}

// runFill works through the images of a fill job: the ones without annotations, in the order of the
// task, up to the limit of the call.
func (s *Service) runFill(ctx context.Context, jobID, chatID string, spec jobSpec) (any, error) {
	r := &recorder{chatID: chatID}
	res := fillAnswer{}
	user, err := s.user(ctx, chatID)
	if err != nil {
		return nil, err
	}
	d, err := s.ownDataset(ctx, chatID, r, spec.DatasetID)
	if err != nil {
		return nil, err
	}
	taskID, err := taskIDOf(d)
	if err != nil {
		return nil, err
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return nil, err
	}
	fn, err := s.usableFunction(ctx, r, ses, user, spec.Function)
	if err != nil {
		return nil, err
	}
	mapping, err := runMapping(spec.Mapping, fn.Labels, d.AnnotationLabels)
	if err != nil {
		return nil, err
	}
	names := mappingNames(mapping)
	t, err := s.C.Task(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10), err)
	if err != nil {
		return nil, err
	}
	labels := t.Labels
	if len(labels) == 0 {
		labels, err = s.C.Labels(ctx, ses, taskID)
		r.cvat("GET /api/labels", err)
		if err != nil {
			return nil, err
		}
	}
	classes := classIDs(labels)
	if len(classes) == 0 {
		return nil, errors.New("the CVAT task of this dataset has no classes")
	}
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		return nil, err
	}
	annotated := annotationsByFrame(ann)
	empty := make([]int, 0, t.Size)
	for frame := 0; frame < t.Size; frame++ {
		if annotated[frame] {
			continue
		}
		empty = append(empty, frame)
	}
	res.SkippedAnnotated = len(annotated)
	if spec.Limit > 0 && len(empty) > spec.Limit {
		empty = empty[:spec.Limit]
	}
	if len(empty) == 0 {
		return res, nil
	}
	// The meta carries the size of every image (for the box check) and its file name; a meta that does
	// not answer leaves both unknown, and a box is checked for finite numbers only (V3).
	meta, merr := s.C.TaskMeta(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/data/meta", merr)
	threshold := float64(spec.MinConfidence) / 100
	var pending []frameAnnotations
	for i, frame := range empty {
		objs, err := s.C.CallFunction(ctx, ses, fn.ID, FunctionCall{Task: taskID, Frame: frame,
			Quality: "original", Threshold: threshold, Mapping: mapping})
		r.cvat("POST /api/lambda/functions/"+fn.ID+" frame "+strconv.Itoa(frame), err)
		switch {
		case errors.Is(err, errBadAnswer):
			// An answer the gateway cannot read (a NaN in the points, another shape) is an image without
			// a usable result: nothing of it is written (V3).
			res.EmptyResults++
		case err != nil:
			return res, err // the job ends with what it wrote so far
		default:
			frameMeta := FrameMeta{}
			if merr == nil {
				frameMeta = meta.Frame(frame)
			}
			tags, shapes := fillObjects(objs, frame, frameMeta, classes, names)
			if len(tags) == 0 && len(shapes) == 0 {
				res.EmptyResults++
			} else {
				pending = append(pending, frameAnnotations{frame: frame, tags: tags, shapes: shapes})
			}
		}
		if len(pending) >= fillBatch || i+1 == len(empty) {
			if err := s.writeFill(ctx, r, ses, taskID, pending, &res); err != nil {
				return res, err
			}
			pending = pending[:0]
		}
		if (i+1)%progressEvery == 0 || i+1 == len(empty) {
			s.jobProgress(ctx, chatID, jobID, i+1, len(empty))
		}
	}
	return res, nil
}

// writeFill writes the collected images into the task. Right before the write it reads the annotations
// of the task again and leaves out every image that carries one meanwhile — the user may annotate in
// CVAT while the job runs, and a machine object next to a manual one is not separable afterwards (E12).
func (s *Service) writeFill(ctx context.Context, r *recorder, ses session, taskID int64, pending []frameAnnotations, res *fillAnswer) error {
	if len(pending) == 0 {
		return nil
	}
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		return err
	}
	taken := annotationsByFrame(ann)
	body := NewAnnotations{Version: 0, Tags: []NewTag{}, Shapes: []NewShape{}, Tracks: []any{}}
	written := 0
	for _, p := range pending {
		if taken[p.frame] {
			res.SkippedAnnotated++
			continue
		}
		body.Tags = append(body.Tags, p.tags...)
		body.Shapes = append(body.Shapes, p.shapes...)
		written++
	}
	if written == 0 {
		return nil
	}
	err = s.C.CreateAnnotations(ctx, ses, taskID, body)
	r.cvat("PATCH /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations action=create", err)
	if err != nil {
		return err
	}
	res.Filled += written
	return nil
}

// fillObjects checks what the function answered for one image and turns it into annotations CVAT takes:
// the class must be a class of the task, the box must be four finite numbers inside the image. What
// fails is dropped and not written; the templates of the gateway answer tags and rectangles only (V3).
func fillObjects(objs []FunctionObject, frame int, meta FrameMeta, classes map[string]int64, mapping map[string]string) (tags []NewTag, shapes []NewShape) {
	for _, o := range objs {
		class := fillClass(o, classes, mapping)
		id, ok := classes[class]
		if !ok {
			continue // CVAT drops a class the call does not map; nothing of it reaches the task
		}
		typ := o.Type
		if typ == "" {
			typ = "tag"
			if len(o.Points) == 4 {
				typ = "rectangle"
			}
		}
		switch typ {
		case "tag":
			tags = append(tags, NewTag{Frame: frame, LabelID: id, Source: sourceAuto, Attributes: []NewAttribute{}})
		case "rectangle":
			p := boxOf(o)
			if p == nil || p[2]-p[0] <= 0 || p[3]-p[1] <= 0 || !insideBox(p, meta) {
				continue
			}
			shapes = append(shapes, NewShape{Frame: frame, LabelID: id, Source: sourceAuto, Type: "rectangle",
				Points: p, Attributes: []NewAttribute{}})
		}
	}
	return tags, shapes
}

// fillClass names the class of an answered object: the class CVAT mapped onto the task first, else the
// class of the function through the mapping of the call, else nothing.
func fillClass(o FunctionObject, classes map[string]int64, mapping map[string]string) string {
	if _, ok := classes[o.Label]; ok {
		return o.Label
	}
	if o.RawLabel != "" {
		if class, ok := mapping[o.RawLabel]; ok {
			return class
		}
	}
	return ""
}

// insideBox reports whether a box lies in the image. Without the size of the image (a meta that does
// not answer) any finite box counts.
func insideBox(p []float64, meta FrameMeta) bool {
	if meta.Width <= 0 || meta.Height <= 0 {
		return true
	}
	return p[0] >= 0 && p[1] >= 0 && p[2] <= float64(meta.Width) && p[3] <= float64(meta.Height)
}

// classIDs maps the classes of a task by name onto the identifier CVAT gave them.
func classIDs(labels []Label) map[string]int64 {
	out := make(map[string]int64, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out[l.Name] = l.ID
		}
	}
	return out
}

// mappingNames turns the mapping of a call (CVAT's form) into plain names.
func mappingNames(m map[string]Descr) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.Name
	}
	return out
}

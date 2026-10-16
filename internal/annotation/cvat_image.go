// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The CVAT calls of the checks and the fills (V3): one function about one image, the jobs of a task
// (for the links into the CVAT UI), the meta of a task's data and writing annotations into a task.
// They live in their own file next to the calls of V1 (cvat.go), which stay what they are.

// Limits of the answers of the calls below.
const (
	maxFunctionAnswerBytes = 4 << 20
	maxJobsBytes           = 4 << 20
	maxMetaBytes           = 8 << 20
)

// FunctionObject is one object a pre-annotation function answered for one image: CVAT rewrites label
// and attributes on the way and leaves everything else alone, so confidence, raw_label and scores
// (V3) arrive as the function wrote them.
type FunctionObject struct {
	// Label is the class after CVAT applied the mapping of the call; a class without a mapping is
	// dropped by CVAT and never appears here.
	Label string `json:"label"`
	// RawLabel is the class as the function names it: the only signal when the mapping of the call is
	// not the mapping of the run (calibration, V3).
	RawLabel string `json:"raw_label"`
	// Type is CVAT's kind of the object: tag (classification) or one of the shape types.
	Type string `json:"type"`
	// Points is the box [x1,y1,x2,y2] of a shape; empty for a tag.
	Points []float64 `json:"points"`
	// Confidence is the score of the object in 0..1; CVAT and the templates answer it as a string.
	Confidence flexFloat `json:"confidence"`
	// Scores are the scores of all classes a classification function answered (raw material of the
	// calibration); empty for a detector.
	Scores []flexFloat `json:"scores"`
}

// flexFloat reads a number that may come as a number or as a string: the templates of V2 write
// confidence as f"{p:.4f}", and CVAT passes it through.
type flexFloat float64

// UnmarshalJSON reads a JSON number, a string holding one (also "NaN" and "inf", which Python's json
// writes) or null. A value that is not a number is an error, so that a broken answer is refused
// instead of counting as zero.
func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fmt.Errorf("%s is not a number", truncate(s, 20))
	}
	*f = flexFloat(v)
	return nil
}

// Float is the value as a float.
func (f flexFloat) Float() float64 { return float64(f) }

// FunctionCall is the body of one call of a function for one image (POST /api/lambda/functions/{id}).
type FunctionCall struct {
	Task int64 `json:"task"`
	// Frame is the number of the image in the task.
	Frame     int     `json:"frame"`
	Quality   string  `json:"quality"`
	Threshold float64 `json:"threshold"`
	// Mapping is CVAT's form of the class mapping; empty lets CVAT use equal names.
	Mapping map[string]Descr `json:"mapping,omitempty"`
}

// errBadAnswer is an answer of a function that the gateway cannot read: no JSON, not a list of objects,
// or a number that no JSON carries (Python's json writes NaN, and a NaN coordinate makes the whole
// answer unreadable). A fill counts such an image as one without a usable result; a check fails on it,
// because a measurement over silently dropped answers would be wrong (V3).
var errBadAnswer = errors.New("the answer of the function is not readable")

// CallFunction asks one function about one image of a task. The answer is the list of objects the
// function found, as the function wrote it (V3 calibrates on it).
func (c *CVAT) CallFunction(ctx context.Context, s session, id string, call FunctionCall) ([]FunctionObject, error) {
	body, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	path := cvatAPI + "/lambda/functions/" + url.PathEscape(id)
	raw, err := c.do(ctx, s, http.MethodPost, path, nil, body, maxFunctionAnswerBytes)
	if err != nil {
		return nil, err
	}
	var out []FunctionObject
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadAnswer, err)
	}
	return out, nil
}

// Job is one annotation job of a task (CVAT splits a task into jobs of consecutive frames). The link
// of an image points at the job that carries it.
type Job struct {
	ID         int64 `json:"id"`
	StartFrame int   `json:"start_frame"`
	StopFrame  int   `json:"stop_frame"`
}

// Jobs lists the jobs of a task; the answer of CVAT is paginated.
func (c *CVAT) Jobs(ctx context.Context, s session, taskID int64) ([]Job, error) {
	q := url.Values{"task_id": {strconv.FormatInt(taskID, 10)}, "page_size": {"100"}}
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/jobs", q, nil, maxJobsBytes)
	if err != nil {
		return nil, err
	}
	var page struct {
		Results []Job `json:"results"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		// A server that answers the list without the page envelope (or an older CVAT) is read as well.
		var list []Job
		if json.Unmarshal(raw, &list) != nil {
			return nil, errors.New("CVAT's job list is not readable")
		}
		return list, nil
	}
	return page.Results, nil
}

// FrameMeta is what the meta of a task's data says about one image: its size and its file name.
type FrameMeta struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Name   string `json:"name"`
}

// TaskMeta is the meta of a task's data: the frames with their size and file name. CVAT may answer the
// meta of the whole task (frames) or of one frame (width, height, name without number); both forms are
// read here, and a meta without either leaves the sizes and the file names unknown — then a box is
// checked for finite numbers only and a link names the image by its number. (Live to confirm, V3.)
type TaskMeta struct {
	Size   int         `json:"size"`
	Width  int         `json:"width"`
	Height int         `json:"height"`
	Name   string      `json:"name"`
	Frames []FrameMeta `json:"frames"`
}

// Frame returns what the meta says about one frame of the task; an unknown frame is zero.
func (m TaskMeta) Frame(n int) FrameMeta {
	if n >= 0 && n < len(m.Frames) {
		return m.Frames[n]
	}
	if len(m.Frames) > 1 {
		return FrameMeta{} // a list that does not cover the frame: nothing is known about it
	}
	return FrameMeta{Width: m.Width, Height: m.Height, Name: m.Name}
}

// TaskMeta reads the meta of a task's data: the file names and the size of every image (V3).
func (c *CVAT) TaskMeta(ctx context.Context, s session, taskID int64) (TaskMeta, error) {
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/tasks/"+strconv.FormatInt(taskID, 10)+"/data/meta", nil, nil, maxMetaBytes)
	if err != nil {
		return TaskMeta{}, err
	}
	var m TaskMeta
	if json.Unmarshal(raw, &m) != nil {
		return TaskMeta{}, errors.New("the meta of the task is not readable")
	}
	return m, nil
}

// NewAttribute is one attribute value of a new annotation.
type NewAttribute struct {
	SpecID int    `json:"spec_id"`
	Value  string `json:"value"`
}

// NewTag is one classification annotation a fill writes (source "auto").
type NewTag struct {
	Frame      int            `json:"frame"`
	LabelID    int64          `json:"label_id"`
	Source     string         `json:"source"`
	Group      int            `json:"group"`
	Attributes []NewAttribute `json:"attributes"`
}

// NewShape is one object annotation a fill writes (source "auto").
type NewShape struct {
	Frame      int            `json:"frame"`
	LabelID    int64          `json:"label_id"`
	Source     string         `json:"source"`
	Type       string         `json:"type"`
	Points     []float64      `json:"points"`
	Occluded   bool           `json:"occluded"`
	ZOrder     int            `json:"z_order"`
	Group      int            `json:"group"`
	Attributes []NewAttribute `json:"attributes"`
}

// NewAnnotations is the body of PATCH /api/tasks/{id}/annotations?action=create. Version is what CVAT
// expects of a writing call (the plan fixes 0, V3).
type NewAnnotations struct {
	Version int        `json:"version"`
	Tags    []NewTag   `json:"tags"`
	Shapes  []NewShape `json:"shapes"`
	Tracks  []any      `json:"tracks"`
}

// AnnotationsDelete is the body of PATCH /api/tasks/{id}/annotations?action=delete (V3b): CVAT deletes
// exactly the objects whose id stands in one of the three lists, whatever their source
// (apps/engine/views.py:1060-1068, apps/dataset_manager/task.py:327-356). What stands in the lists is
// the gateway's own choice, never the agent's (see rollback.go).
type AnnotationsDelete struct {
	Tags   []int64 `json:"tags"`
	Shapes []int64 `json:"shapes"`
	Tracks []int64 `json:"tracks"`
}

// DeleteAnnotations removes exactly the named annotations of a task (V3b). CVAT itself does not look at
// the source of an object, so the caller passes only what the gateway wrote and found unchanged.
func (c *CVAT) DeleteAnnotations(ctx context.Context, s session, taskID int64, a AnnotationsDelete) error {
	if a.Tags == nil {
		a.Tags = []int64{}
	}
	if a.Shapes == nil {
		a.Shapes = []int64{}
	}
	if a.Tracks == nil {
		a.Tracks = []int64{}
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	q := url.Values{"action": {"delete"}}
	_, err = c.do(ctx, s, http.MethodPatch, cvatAPI+"/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", q, body, maxJSONBytes)
	return err
}

// CreateAnnotations writes new annotations into a task and leaves everything else alone
// (action=create). The body carries whole images: the caller checks before the call that the images
// it touches are still empty (V3).
func (c *CVAT) CreateAnnotations(ctx context.Context, s session, taskID int64, a NewAnnotations) error {
	if a.Tags == nil {
		a.Tags = []NewTag{}
	}
	if a.Shapes == nil {
		a.Shapes = []NewShape{}
	}
	if a.Tracks == nil {
		a.Tracks = []any{}
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	q := url.Values{"action": {"create"}}
	_, err = c.do(ctx, s, http.MethodPatch, cvatAPI+"/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", q, body, maxJSONBytes)
	return err
}

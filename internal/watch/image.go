// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// A built inference image (A-E2): POST /container-images/build builds the image in the background and
// answers 202 with Location: /tasks/<id>. The task ends when the image is in the registry; what the
// build produced is the image <repository>:<tag>, which the call's body names (the tool puts the
// owner's repository in it, so the gateway does not have to read the model for it).
const kindImageBuild = "image_build"

// buildPath is the call that starts a build.
var buildPath = regexp.MustCompile(`^/container-images/build$`)

func init() {
	Register(Kind{Name: kindImageBuild, Resource: delegation.ContainerImage, Poll: pollImageBuild})
	RegisterRoute(http.MethodPost, buildPath, kindImageBuild, chainedTaskRef(chainImage, imageRefFromRequest))
}

// imageRefFromRequest reads "<repository>:<tag>" out of the body of the build.
func imageRefFromRequest(req platform.Request) (string, bool) {
	if len(req.Body) == 0 {
		return "", false
	}
	var doc struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
	}
	if json.Unmarshal(req.Body, &doc) != nil || doc.Repository == "" || doc.Tag == "" {
		return "", false
	}
	return doc.Repository + ":" + doc.Tag, true
}

// pollImageBuild reads the build task; the answer carries the image as an output once it is done, so
// that the card can name what the build produced.
func pollImageBuild(ctx context.Context, j Job, read Reader) (Progress, error) {
	taskID, _, target := splitTaskRef(j.Ref)
	if taskID == "" {
		return Progress{}, fmt.Errorf("job %s names no task", j.ID)
	}
	doc, err := getTask(ctx, read, taskID)
	if err != nil {
		return Progress{}, err
	}
	p := Progress{State: StateRunning, Percent: Percent(doc.Completion), Message: oneLine(doc.Message)}
	switch doc.Status {
	case taskCompleted:
		p.State = StateDone
		if target != "" {
			p.Outputs = []Output{{Resource: "container_image", ID: target, Label: target}}
		}
	case taskFailed:
		p.State = StateFailed
	}
	return p, nil
}

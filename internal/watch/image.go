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
	"strconv"
	"strings"
	"time"

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

// registryWait is how long a finished build may take to show up in the registry listing (the registry
// webhook creates the row, not the build task).
const registryWait = 10 * time.Minute

// watchClock is the time source of the registry wait (tests replace it).
var watchClock = time.Now

// splitRegistryRef takes "registry:<repository>:<tag>@<unix>" apart (the last "@" divides).
func splitRegistryRef(ref string) (target string, since time.Time, ok bool) {
	rest, found := strings.CutPrefix(ref, "registry:")
	if !found {
		return "", time.Time{}, false
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return "", time.Time{}, false
	}
	n, err := strconv.ParseInt(rest[at+1:], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return rest[:at], time.Unix(n, 0), true
}

// imageIDs reads the numeric ids the registry lists for an image reference.
func imageIDs(ctx context.Context, read Reader, target string) ([]string, error) {
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/container-images/" + target})
	if err != nil {
		return nil, err
	}
	if res.HTTPStatus == http.StatusNotFound {
		return nil, nil
	}
	if res.Status == "error" || res.HTTPStatus >= 400 {
		return nil, fmt.Errorf("image %s not readable: %s", target, res.Message)
	}
	var entries []struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Body), &entries); err != nil {
		return nil, nil
	}
	var ids []string
	for _, e := range entries {
		if e.ID != "" && digitsRe.MatchString(e.ID.String()) {
			ids = append(ids, e.ID.String())
		}
	}
	return ids, nil
}

// imageDone fills the finished progress of a build from the registry listing.
func imageDone(p *Progress, target string, ids []string) {
	p.State = StateDone
	p.Outputs = []Output{{Resource: "container_image", ID: target, Label: target}}
	switch len(ids) {
	case 1:
		p.Created = &Output{Resource: delegation.ContainerImage, ID: ids[0], Label: target}
	default:
		p.Message = "the registry lists several images for " + target
	}
}

// pollImageBuild reads the build task; the answer carries the image as an output once it is done, so
// that the card can name what the build produced. Once the task is completed, the registry's own
// listing gives the image id a later plan step means; the registry row may come a little later than
// the task, so the same row is then watched on with a "registry:" reference.
func pollImageBuild(ctx context.Context, j Job, read Reader) (Progress, error) {
	if target, since, ok := splitRegistryRef(j.Ref); ok {
		p := Progress{State: StateRunning}
		ids, err := imageIDs(ctx, read, target)
		if err != nil {
			return Progress{}, err
		}
		switch {
		case len(ids) > 0:
			imageDone(&p, target, ids)
		case watchClock().Sub(since) > registryWait:
			p.State = StateFailed
			p.Message = "the registry lists no image " + target + " 10 minutes after the build"
		default:
			p.Message = "image built; waiting for the registry to list " + target
			p.Next = &Job{Kind: kindImageBuild, Ref: j.Ref}
		}
		return p, nil
	}
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
		if target == "" {
			p.State = StateDone
			break
		}
		ids, err := imageIDs(ctx, read, target)
		if err != nil {
			return Progress{}, err
		}
		if len(ids) == 0 {
			p.Message = "image built; waiting for the registry to list " + target
			p.Next = &Job{Kind: kindImageBuild, Ref: "registry:" + target + "@" + strconv.FormatInt(watchClock().Unix(), 10)}
			break
		}
		imageDone(&p, target, ids)
	case taskFailed:
		p.State = StateFailed
	}
	return p, nil
}

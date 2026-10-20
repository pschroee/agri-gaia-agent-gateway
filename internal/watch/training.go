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

	"agw/internal/delegation"
	"agw/internal/platform"
)

// A started training container (A-E2): POST /train/containers/<id>/run starts Docker's container and
// answers 202 with a task that ends as soon as the container runs. The training itself is then watched
// through the container: GET /train/containers/<id>/status (status and score) and
// GET /train/containers/<id>/logs?tail=<n>.
//
// The status comes straight from Docker (get_container_status reads container.status): created,
// running, exited, paused, restarting, dead. A container that exited is done when it left a score or
// its log shows no error; anything else is a failure. (Checked against the backend, 16.10.2026:
// routers/train.py:456 returns {"status", "score"}, util/train.py:222 returns the Docker status.)
const kindTrainingRun = "training_run"

// runPath is the call that starts a container; the task it answers with is watched as a platform_task
// first, then — through the chain — as this kind (see task.go).
var runPath = regexp.MustCompile(`^/train/containers/[^/]+/run$`)

// containerPath matches the calls of the kind; the reference is the container id.
var containerPath = regexp.MustCompile(`^/train/containers/([^/]+)`)

func init() {
	Register(Kind{Name: kindTrainingRun, Resource: delegation.Training, Poll: pollTrainingRun, Stop: stopTrainingRun})
	RegisterRoute(http.MethodPost, runPath, kindPlatformTask, chainedTaskRef(chainTraining, containerIDFromRequest))
}

// containerIDFromRequest reads the container id out of the path of the call that started it.
func containerIDFromRequest(req platform.Request) (string, bool) {
	m := containerPath.FindStringSubmatch(req.Path)
	if m == nil || m[1] == "" {
		return "", false
	}
	return m[1], true
}

// trainingDoc is the answer to GET /train/containers/<id>/status.
type trainingDoc struct {
	Status string   `json:"status"`
	Score  *float64 `json:"score"`
}

// pollTrainingRun reads the container's status and, for a container that is still running, the end of
// its log. The percent stays unknown: a training without tensor metadata reports none, and epochs
// parsed out of the log would depend on the template (D2.4).
func pollTrainingRun(ctx context.Context, j Job, read Reader) (Progress, error) {
	if j.Ref == "" {
		return Progress{}, fmt.Errorf("job %s names no container", j.ID)
	}
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/train/containers/" + j.Ref + "/status"})
	if err != nil {
		return Progress{}, err
	}
	if res.Status == "error" || res.HTTPStatus >= 400 {
		// A 404 means the training container was removed while it was observed (issue #140): the job ends,
		// every other status may pass.
		err := fmt.Errorf("training container %s not readable: %s", j.Ref, res.Message)
		if DecidedByStatus(res.HTTPStatus) {
			return Progress{}, Decided(err)
		}
		return Progress{}, err
	}
	var doc trainingDoc
	if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
		return Progress{}, Decidedf("training container %s: %v", j.Ref, err)
	}
	tail, terr := trainingLogTail(ctx, read, j.Ref)
	p := Progress{State: StateRunning, Message: oneLine(trainingMessage(doc)), LogTail: tail}
	switch doc.Status {
	case "exited":
		if doc.Score != nil || (terr == nil && !logShowsError(tail)) {
			p.State = StateDone
		} else {
			p.State = StateFailed
		}
	case "dead":
		p.State = StateFailed
	}
	return p, nil
}

// trainingMessage is the one line the job shows about a training container: the Docker status and, if
// the platform has one, its score.
func trainingMessage(doc trainingDoc) string {
	if doc.Score != nil {
		return fmt.Sprintf("%s, score %s", doc.Status, strconv.FormatFloat(*doc.Score, 'f', -1, 64))
	}
	return doc.Status
}

// logShowsError reports whether the log tail names an error. It is a rough test on purpose: it only
// decides whether an exited container without a score counts as failed, and the watcher's message
// carries the log tail so that the agent can look.
func logShowsError(tail string) bool {
	return regexp.MustCompile(`(?i)\b(error|traceback|exception|failed)\b`).MatchString(tail)
}

// trainingLogTail reads the last lines of the container's log; a failure is not fatal, the status
// decides.
func trainingLogTail(ctx context.Context, read Reader, containerID string) (string, error) {
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/train/containers/" + containerID + "/logs",
		Query: map[string]string{"tail": strconv.Itoa(MaxLogLines)}})
	if err != nil {
		return "", err
	}
	if res.Status == "error" || res.HTTPStatus >= 400 {
		return "", fmt.Errorf("logs of %s not readable: %s", containerID, res.Message)
	}
	var doc struct {
		Logs string `json:"logs"`
	}
	if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
		return "", err
	}
	return Tail(doc.Logs), nil
}

// stopTrainingRun is the call that stops the container (POST /train/containers/<id>/stop); the platform
// answers 202 with a task of its own, which the watcher does not follow.
func stopTrainingRun(j Job) (platform.Request, bool) {
	if j.Ref == "" {
		return platform.Request{}, false
	}
	return platform.Request{Method: http.MethodPost, Path: "/train/containers/" + j.Ref + "/stop"}, true
}

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
	"strings"

	"agw/internal/platform"
)

// The platform's own background tasks (A-E2): POST /train/config builds a training container,
// POST /container-images/build builds an inference image. Both answer 202 with Location: /tasks/<id>,
// and the platform reports status (created, inprogress, completed, failed), completion_percentage
// (a fraction 0..1) and a message.
//
// A task that only starts something is watched on (kindChain): POST /train/containers/<id>/run ends
// when the container is started, not when the training is done, so the same row keeps being watched as
// a training_run (the plan's assumption, (ungeprüft) in issue #99, matches the backend: the task's
// function only calls container.start()).
const kindPlatformTask = "platform_task"

// taskStartPath is the call that starts a task of this kind; the reference is the task id from Location.
var taskStartPath = regexp.MustCompile(`^/train/config$`)

// taskChains maps the marker in a chained reference to the kind that observes the job itself.
const (
	chainTraining = "train"
	chainImage    = "image"
)

func init() {
	Register(Kind{Name: kindPlatformTask, Poll: pollPlatformTask})
	RegisterRoute(http.MethodPost, taskStartPath, kindPlatformTask, taskRef)
}

// taskRef reads the task id out of the answer's Location ("/tasks/<id>", optionally with a chain the
// caller built: "task:<id>|train:<container>").
func taskRef(_ platform.Request, res platform.Result) (string, bool) {
	id, ok := taskIDFromLocation(res.Location)
	if !ok {
		return "", false
	}
	return "task:" + id, true
}

// taskIDFromLocation reads the task id out of a Location header ("/tasks/12", absolute or with a query).
func taskIDFromLocation(loc string) (string, bool) {
	m := regexp.MustCompile(`/tasks/([0-9]+)`).FindStringSubmatch(loc)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// taskRefAt is taskRef plus a chain the kind wants to follow on: the task ends first, then the chained
// job is observed with the kind named by the marker.
func chainedTaskRef(chain string, ref func(req platform.Request) (string, bool)) Ref {
	return func(req platform.Request, res platform.Result) (string, bool) {
		id, ok := taskIDFromLocation(res.Location)
		if !ok {
			return "", false
		}
		out := "task:" + id
		if chain == "" {
			return out, true
		}
		rest, ok := ref(req)
		if !ok {
			return "", false
		}
		return out + "|" + chain + ":" + rest, true
	}
}

// splitTaskRef takes a platform_task reference apart: the task id and, if the task only starts
// something, the chain that is watched afterwards.
func splitTaskRef(ref string) (taskID, chain, target string) {
	rest, ok := strings.CutPrefix(ref, "task:")
	if !ok {
		rest = ref
	}
	head, tail, _ := strings.Cut(rest, "|")
	chain, target, _ = strings.Cut(tail, ":")
	return head, chain, target
}

// Status values of a platform task (db/models.py: TaskStatus).
const (
	taskInProgress = "inprogress"
	taskCompleted  = "completed"
	taskFailed     = "failed"
)

// taskDoc is the platform's answer to GET /tasks/<id>.
type taskDoc struct {
	Status     string  `json:"status"`
	Completion float64 `json:"completion_percentage"`
	Message    string  `json:"message"`
}

// pollPlatformTask reads the state of the task and, once it ends, hands over to the chained kind.
func pollPlatformTask(ctx context.Context, j Job, read Reader) (Progress, error) {
	taskID, chain, target := splitTaskRef(j.Ref)
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
		if chain == "" {
			p.State = StateDone
			return p, nil
		}
		// The task only started the job: the same row is watched on as that job from now on.
		p.Next = nextJob(chain, target)
		if p.Next == nil {
			p.State = StateDone
		}
	case taskFailed:
		p.State = StateFailed
	}
	return p, nil
}

// nextJob names the kind and the reference that observes what a finished task started.
func nextJob(chain, target string) *Job {
	switch chain {
	case chainTraining:
		if target == "" {
			return nil
		}
		return &Job{Kind: kindTrainingRun, Ref: target}
	case chainImage:
		if target == "" {
			return nil
		}
		return &Job{Kind: kindImageBuild, Ref: target}
	}
	return nil
}

// getTask reads one task of the platform.
func getTask(ctx context.Context, read Reader, taskID string) (taskDoc, error) {
	var doc taskDoc
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/tasks/" + taskID})
	if err != nil {
		return doc, err
	}
	if res.Status == "error" || res.HTTPStatus >= 400 {
		return doc, fmt.Errorf("task %s not readable: %s", taskID, res.Message)
	}
	if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
		return doc, fmt.Errorf("task %s: %v", taskID, err)
	}
	return doc, nil
}

// oneLine flattens a platform message to one line of at most 200 runes (D2.4/D2.5).
func oneLine(s string) string { return ClipRunes(strings.Join(strings.Fields(s), " "), MaxMessage) }

// ClipRunes cuts a text to n runes; a longer one ends with an ellipsis.
func ClipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …"
}

// Tail keeps the end of a text: at most MaxLogLines lines and MaxLogRunes runes (D2.5). What a job
// stores is bounded, whatever the platform answers.
func Tail(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > MaxLogLines {
		lines = lines[len(lines)-MaxLogLines:]
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > MaxLogRunes {
		out = "…" + string(r[len(r)-MaxLogRunes:])
	}
	return out
}

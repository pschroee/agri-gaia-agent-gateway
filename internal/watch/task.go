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

	"agw/internal/delegation"
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
	// chainConfig marks the build of a training container: the task is the whole job, but its message
	// names the container it built, and the gateway checks that container in the platform's list.
	chainConfig = "config"
)

func init() {
	Register(Kind{Name: kindPlatformTask, Resource: delegation.Task, Poll: pollPlatformTask})
	RegisterRoute(http.MethodPost, taskStartPath, kindPlatformTask, taskRef)
}

// taskRef reads the task id out of the answer's Location ("/tasks/<id>"). When the body names the
// dataset, the reference carries it ("task:<id>|config:<dataset_id>"), so that the container the
// build reports can be checked against it.
func taskRef(req platform.Request, res platform.Result) (string, bool) {
	id, ok := taskIDFromLocation(res.Location)
	if !ok {
		return "", false
	}
	if ds := datasetIDFromRequest(req); ds != "" {
		return "task:" + id + "|" + chainConfig + ":" + ds, true
	}
	return "task:" + id, true
}

// datasetIDFromRequest reads dataset_id out of the body of a training configuration (a number or a
// string of digits); "" when it names none.
func datasetIDFromRequest(req platform.Request) string {
	if len(req.Body) == 0 {
		return ""
	}
	var doc struct {
		DatasetID json.Number `json:"dataset_id"`
	}
	dec := json.NewDecoder(strings.NewReader(string(req.Body)))
	dec.UseNumber()
	if dec.Decode(&doc) != nil || !digitsRe.MatchString(doc.DatasetID.String()) {
		return ""
	}
	return doc.DatasetID.String()
}

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

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
	Title      string  `json:"title"`
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
		if chain == chainConfig {
			p.State = StateDone
			created, msg, err := builtContainer(ctx, read, doc, target)
			if err != nil {
				return Progress{}, err
			}
			p.Created = created
			if msg != "" {
				p.Message = oneLine(msg)
			}
			if p.Created != nil {
				p.Outputs = append(p.Outputs, *p.Created)
			}
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

// builtContainerRe is the message the backend writes into the task once it created the container row
// (routers/train.py, train_container_created_message).
var builtContainerRe = regexp.MustCompile(`^Train Container #([0-9]+) created\.$`)

// builtContainer works out the training container a finished build task created: the id comes from the
// task's message and is checked against the platform's own list (and the dataset the call named). A nil
// object means the platform named none, or none that the list shows; msg then says why (empty when the
// task simply had no message). A failed read of the list is an error, so that the job is read again.
func builtContainer(ctx context.Context, read Reader, doc taskDoc, dataset string) (*Output, string, error) {
	m := builtContainerRe.FindStringSubmatch(strings.TrimSpace(doc.Message))
	if m == nil {
		return nil, "", nil
	}
	id := m[1]
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/train/containers", Query: map[string]string{"limit": "1000"}})
	if err != nil {
		return nil, "", err
	}
	if res.Status == "error" || res.HTTPStatus >= 400 {
		return nil, "", fmt.Errorf("training containers not readable: %s", res.Message)
	}
	var rows []struct {
		ID        json.Number `json:"id"`
		DatasetID json.Number `json:"dataset_id"`
	}
	if err := json.Unmarshal([]byte(res.Body), &rows); err != nil {
		return nil, "", fmt.Errorf("training containers: %v", err)
	}
	for _, r := range rows {
		if r.ID.String() != id {
			continue
		}
		if dataset == "" || r.DatasetID.String() == dataset {
			return &Output{Resource: delegation.Training, ID: id, Label: "training container " + id}, "", nil
		}
	}
	return nil, fmt.Sprintf("the platform names training container %s, but the list does not show it for dataset %s", id, dataset), nil
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
		// A 404 says the task is gone: no further attempt can make it readable (issue #140). Everything
		// else (5xx, a lost login) may pass and is retried.
		err := fmt.Errorf("task %s not readable: %s", taskID, res.Message)
		if DecidedByStatus(res.HTTPStatus) {
			return doc, Decided(err)
		}
		return doc, err
	}
	if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
		// The answer is no task any more (a deleted one answers an empty body): decided as well.
		return doc, Decidedf("task %s: %v", taskID, err)
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
// stores is bounded, whatever the platform answers. Progress bars are collapsed first (CollapseProgress,
// issue #178), so that one redrawn bar does not fill the tail.
func Tail(s string) string {
	s = CollapseProgress(s)
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

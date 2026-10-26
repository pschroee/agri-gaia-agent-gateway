// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/search"
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

// pollTrainingRun reads the container's status, the end of its log and, since issue #168, the figures of the
// run (GET /train/containers/<id>/metrics, written by the template as metrics.jsonl). The percent comes from
// those figures (epoch / epochs); without them it stays unknown, because epochs parsed out of the log would depend
// on the template (D2.4).
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
	collapsed, terr := trainingLogTail(ctx, read, j.Ref)
	p := Progress{State: StateRunning, Message: oneLine(trainingMessage(doc)), LogTail: Tail(collapsed)}
	p.Metrics = trainingMetrics(ctx, read, j.Ref)
	switch doc.Status {
	case "exited":
		if doc.Score != nil || (terr == nil && !logShowsError(collapsed)) {
			p.State = StateDone
		} else {
			p.State = StateFailed
		}
	case "dead":
		p.State = StateFailed
	}
	p.Percent = MetricsPercent(p.Metrics, p.State == StateDone)
	return p, nil
}

// metricsDoc is the answer to GET /train/containers/<id>/metrics (issue #168, backend fork): the lines of
// metrics.jsonl as objects, the GPU samples as objects, and whether the template writes metrics at all.
type metricsDoc struct {
	Supported    bool              `json:"supported"`
	Epochs       []json.RawMessage `json:"epochs"`
	GPU          []metricsGPUDoc   `json:"gpu"`
	GPUAvailable bool              `json:"gpu_available"`
}

type metricsGPUDoc struct {
	At         string  `json:"at"`
	Util       float64 `json:"util"`
	MemUsedMB  float64 `json:"mem_used_mb"`
	MemTotalMB float64 `json:"mem_total_mb"`
}

// trainingMetrics reads the figures of a training container. A failure is not fatal (the status decides): an
// older backend without the route, a container without figures or an unreadable answer give nil. What the
// platform sends is read like a file the run wrote: line by line through the parsers of internal/search.
func trainingMetrics(ctx context.Context, read Reader, containerID string) *Metrics {
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/train/containers/" + containerID + "/metrics"})
	if err != nil || res.Status == "error" || res.HTTPStatus >= 400 {
		return nil
	}
	var doc metricsDoc
	if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
		return nil
	}
	m := &Metrics{Supported: doc.Supported, GPUAvailable: doc.GPUAvailable}
	// Each epoch object goes through the same line parser as a file; an element that is no object falls away there.
	var lines []byte
	for _, e := range doc.Epochs {
		lines = append(lines, bytes.TrimSpace(e)...)
		lines = append(lines, '\n')
	}
	m.Epochs = search.ParseMetricsJSONL(lines)
	if len(doc.GPU) > search.MaxGPULines {
		doc.GPU = doc.GPU[len(doc.GPU)-search.MaxGPULines:]
	}
	for _, g := range doc.GPU {
		if s, ok := search.GPUSampleOf(g.At, fmt.Sprint(g.Util), wholeText(g.MemUsedMB), wholeText(g.MemTotalMB)); ok {
			m.GPU = append(m.GPU, s)
		}
	}
	return m
}

// wholeText writes a number as a whole-number text, "x" (which no parser takes) for one that is not.
func wholeText(f float64) string {
	if f != math.Trunc(f) || f < 0 || f > 1<<31 {
		return "x"
	}
	return strconv.FormatInt(int64(f), 10)
}

// MetricsPercent is the percent of a run from its figures: the last epoch of the epochs the run names, at most 99
// while it runs and 100 once it ended well (issue #168; with it the ETA of the watcher works for the template).
// nil while no line names the number of epochs.
func MetricsPercent(m *Metrics, done bool) *float64 {
	if m == nil || len(m.Epochs) == 0 {
		return nil
	}
	last := m.Epochs[len(m.Epochs)-1]
	if done {
		p := 100.0
		return &p
	}
	if last.Epochs < 1 {
		return nil
	}
	p := math.Min(float64(last.Epoch)/float64(last.Epochs)*100, 99)
	return &p
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
	return logErrorPattern.MatchString(tail)
}

var logErrorPattern = regexp.MustCompile(`(?i)\b(error|traceback|exception|failed)\b`)

// LastErrorLine is the last line of the log tail that names an error (issue #227), trimmed and clipped; empty when no
// line does.
func LastErrorLine(tail string) string {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && logErrorPattern.MatchString(l) {
			return ClipRunes(l, 200)
		}
	}
	return ""
}

// trainingLogReadLines is how many log lines the watcher reads: a traceback may sit above many progress
// lines, and the error check runs on the collapsed whole (issue #178).
const trainingLogReadLines = 100

// trainingLogTail reads the last lines of the container's log and collapses progress bars; a failure is
// not fatal, the status decides.
func trainingLogTail(ctx context.Context, read Reader, containerID string) (string, error) {
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/train/containers/" + containerID + "/logs",
		Query: map[string]string{"tail": strconv.Itoa(trainingLogReadLines)}})
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
	return CollapseProgress(doc.Logs), nil
}

// stopTrainingRun is the call that stops the container (POST /train/containers/<id>/stop); the platform
// answers 202 with a task of its own, which the watcher does not follow.
func stopTrainingRun(j Job) (platform.Request, bool) {
	if j.Ref == "" {
		return platform.Request{}, false
	}
	return platform.Request{Method: http.MethodPost, Path: "/train/containers/" + j.Ref + "/stop"}, true
}

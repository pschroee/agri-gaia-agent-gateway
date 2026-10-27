// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package watch holds the skeleton for observing long-running jobs on the platform (G1.7): the types
// and the two registrations the other plans (own code C-E3, edge D-E2, pre-annotation V1/V3, autonomy
// A-E2) use, so that their kinds can grow next to each other in their own files. It holds **no** loop
// and no table: the loop and the table watch_jobs come with the autonomy plan (A-E2). The first kind
// that lives in the package itself is edge_deployment (edge.go, D-E2); the others register their kind
// from their own package (notebook, pre-annotation). Progress never comes from the agent.
package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/search"
)

// States of a job, as its row in watch_jobs keeps them (store.JobRunning, JobDone, JobFailed).
const (
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Limits of what one poll writes down (D2.4, D2.5).
const (
	MaxMessage  = 200  // runes of the platform's message
	MaxLogLines = 20   // lines of the log tail
	MaxLogRunes = 4000 // runes of the log tail
)

// Progress is the state of an observed job as the watcher works it out from the platform.
type Progress struct {
	State   string   // "running" | "done" | "failed"
	Percent *float64 // 0..100, nil = unknown
	Message string   // one line, from the platform, at most 200 runes
	// Phase is the text the history of the job (issue #246) records when the kind works out its phases itself
	// (a training run); empty: the history takes Message.
	Phase   string
	LogTail string   // at most 20 lines / 4000 runes
	Outputs []Output // objects the job produced: {Resource, ID, Label}
	// Created is the object a later plan step means by {{stepN.id}}; only with StateDone, read by the
	// gateway from the platform, never from the agent.
	Created *Output
	// Metrics are the figures a training or a notebook run wrote (epochs, GPU samples; issue #168); nil for the
	// kinds that have none and when nothing could be read.
	Metrics *Metrics
	// Link is what the user opens for this job, when the kind knows such an address (the kind
	// notebook_run carries the run in the user's JupyterHub). Empty for the kinds without one, and the
	// card builds no address of its own (own code C-E4).
	Link string
	// RunStartedAt and RunEndedAt are the run's own start and end, as the run wrote them; nil when the kind
	// does not know them (issue #203).
	RunStartedAt, RunEndedAt *time.Time
	// Next (A-E2) replaces the kind and the reference of the running job: the observed task only
	// started another job (a started training container, a built image), and the same row is observed
	// on as that job. nil for every kind that follows its job to its end.
	Next *Job
}

// Metrics are what the platform (training) or the run directory (notebook) says about the course of a run: the
// lines of metrics.jsonl and the GPU samples, already parsed and checked by internal/search. The watcher reads
// the whole series every time (at most 1 000 epochs and 2 000 samples); the one that stores them knows what is new.
type Metrics struct {
	Epochs []search.Epoch
	GPU    []search.GPUSample
	// Supported: the training template writes metrics.jsonl (label agrigaia.metrics); a notebook run is always true.
	Supported bool
	// GPUAvailable: the run sampled a GPU (gpu.csv exists).
	GPUAvailable bool
	// Params are the parameters of a notebook run (its params.json, canonical JSON object); nil when it has none.
	Params json.RawMessage
}

// Output is an object a job produced. Size is its size in bytes when the kind knows it (the result files
// of a notebook run, own code C-E4) and 0 for the kinds that do not.
type Output struct {
	Resource string
	ID       string
	Label    string
	Size     int64
}

// Job is a row of watch_jobs: what is observed, for which chat, by which kind.
type Job struct{ ID, ChatID, Kind, Ref string }

// Reader performs the reading calls of the chat's user; a kind polls the platform through it.
type Reader func(ctx context.Context, r platform.Request) (platform.Result, error)

// Kind is one sort of observed job. Poll asks the platform for the state; Stop names the call that
// stops the job, if it can be stopped.
type Kind struct {
	Name string
	// Resource is what the job changes on the platform (#167); it feeds platform_changed.
	Resource string
	Poll     func(ctx context.Context, j Job, read Reader) (Progress, error)
	Stop     func(j Job) (platform.Request, bool)
	// EndNote: the end of a job of this kind is told to the agent outside a plan (#252). The start is not: the
	// call that starts such a job has its own answer.
	EndNote bool
}

// Ref reads the job's reference (e.g. the task id) out of a call and its answer.
type Ref func(req platform.Request, res platform.Result) (string, bool)

var (
	mu     sync.Mutex
	kinds  = map[string]Kind{}
	routes []route
)

// route connects a call of the agent with the kind that observes it.
type route struct {
	method string
	path   *regexp.Regexp
	kind   string
	ref    Ref
}

// Lookup returns the kind registered under a name (the loop of A-E2 polls by the name its row carries);
// ok is false for a name this build does not know.
func Lookup(name string) (Kind, bool) {
	mu.Lock()
	defer mu.Unlock()
	k, ok := kinds[name]
	return k, ok
}

// Register registers a kind (G1.7). A kind of the same name replaces the previous one.
func Register(k Kind) {
	mu.Lock()
	defer mu.Unlock()
	kinds[k.Name] = k
}

// RegisterRoute registers which call starts a job of a kind and how its reference is found (G1.7).
// Each kind brings its own route in its own init(); the routes are checked in the order they were
// registered.
func RegisterRoute(method string, path *regexp.Regexp, kind string, ref Ref) {
	mu.Lock()
	defer mu.Unlock()
	routes = append(routes, route{method: method, path: path, kind: kind, ref: ref})
}

// For names the kind and the reference of a call that started a job; ok is false when no registered
// route matches or the route finds no reference. access is the classification of the call, so that a
// kind can be bound to the resource it works on.
func For(access delegation.Access, req platform.Request, res platform.Result) (kind, ref string, ok bool) {
	mu.Lock()
	list := append([]route(nil), routes...)
	mu.Unlock()
	for _, r := range list {
		if r.method != req.Method || r.path == nil || !r.path.MatchString(req.Path) || r.ref == nil {
			continue
		}
		if ref, found := r.ref(req, res); found {
			return r.kind, ref, true
		}
	}
	return "", "", false
}

// outsidePlanKinds are the kinds whose end the gateway tells the agent when no plan step waits for them (#212).
// A search's trials and the notebook and edge jobs have notes of their own.
var outsidePlanKinds = map[string]bool{kindPlatformTask: true, kindTrainingRun: true, kindImageBuild: true}

// AnnouncedOutsidePlan reports whether a job of this kind outside a plan gets a note in the answer of the call
// that started it and a note at its end (#212).
func AnnouncedOutsidePlan(kind string) bool { return outsidePlanKinds[kind] }

// TellsEndOutsidePlan reports whether the end of a job of this kind outside a plan is told to the agent: the kinds
// of AnnouncedOutsidePlan, and every registered kind that asks for it with Kind.EndNote (#252).
func TellsEndOutsidePlan(kind string) bool {
	if outsidePlanKinds[kind] {
		return true
	}
	k, ok := Lookup(kind)
	return ok && k.EndNote
}

var (
	taskReadRe     = regexp.MustCompile(`^/tasks/([0-9]+)$`)
	trainingReadRe = regexp.MustCompile(`^/train/containers/([0-9]+)/(?:status|logs|metrics)$`)
)

// ReadOf recognises a read of a job the agent might poll: GET /tasks/{id} gives (platform_task, id), GET
// /train/containers/{id}/status, /logs or /metrics gives (training_run, id).
func ReadOf(req platform.Request) (kind, id string, ok bool) {
	if req.Method != http.MethodGet {
		return "", "", false
	}
	if m := taskReadRe.FindStringSubmatch(req.Path); m != nil {
		return kindPlatformTask, m[1], true
	}
	if m := trainingReadRe.FindStringSubmatch(req.Path); m != nil {
		return kindTrainingRun, m[1], true
	}
	return "", "", false
}

// TaskIDOf is the platform task a job's reference names ("task:12|train:7" gives "12"; a plain id is its own).
func TaskIDOf(ref string) string {
	id, _, _ := splitTaskRef(ref)
	return id
}

// ReadMatches reports whether a read recognised by ReadOf (readKind, id) polls the job of this kind and
// reference: a task read matches the jobs that follow a task (platform_task, image_build), a training read the
// run of that container, also while the job still follows the task that starts it ("task:12|train:7").
func ReadMatches(jobKind, jobRef, readKind, id string) bool {
	task, chain, target := splitTaskRef(jobRef)
	switch readKind {
	case kindPlatformTask:
		return (jobKind == kindPlatformTask || jobKind == kindImageBuild) && task == id
	case kindTrainingRun:
		return (jobKind == kindTrainingRun && jobRef == id) ||
			(jobKind == kindPlatformTask && chain == chainTraining && target == id)
	}
	return false
}

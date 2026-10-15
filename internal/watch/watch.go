// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package watch holds the skeleton for observing long-running jobs on the platform (G1.7): the types
// and the two registrations the other plans (own code C-E3, edge D-E2, pre-annotation V1/V3, autonomy
// A-E2) use, so that their kinds can grow next to each other in their own files. This package holds
// **no** loop, no table and no kind: the loop, the table watch_jobs and the first kinds come with the
// autonomy plan (A-E2). Progress never comes from the agent.
package watch

import (
	"context"
	"regexp"
	"sync"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// Progress is the state of an observed job as the watcher works it out from the platform.
type Progress struct {
	State   string   // "running" | "done" | "failed"
	Percent *float64 // 0..100, nil = unknown
	Message string   // one line, from the platform, at most 200 runes
	LogTail string   // at most 20 lines / 4000 runes
	Outputs []Output // objects the job produced: {Resource, ID, Label}
}

// Output is an object a job produced.
type Output struct{ Resource, ID, Label string }

// Job is a row of watch_jobs: what is observed, for which chat, by which kind.
type Job struct{ ID, ChatID, Kind, Ref string }

// Reader performs the reading calls of the chat's user; a kind polls the platform through it.
type Reader func(ctx context.Context, r platform.Request) (platform.Result, error)

// Kind is one sort of observed job. Poll asks the platform for the state; Stop names the call that
// stops the job, if it can be stopped.
type Kind struct {
	Name string
	Poll func(ctx context.Context, j Job, read Reader) (Progress, error)
	Stop func(j Job) (platform.Request, bool)
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

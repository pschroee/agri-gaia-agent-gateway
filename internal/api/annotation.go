// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agw/internal/annotation"
	"agw/internal/platform"
	"agw/internal/store"
)

// The annotation route of a chat (V4, issue #112): the cards in the history load the runs and jobs of
// pre-annotation from here, with the result a finished job wrote and the place in the history. Own
// chats only; the route reads the database and never wakes the chat, so a card of a dormant chat
// loads as well.
//
// Progress and state are the watcher's (autonomy A-E2, V4 decision): the rows of annotation_runs and
// annotation_jobs carry what the run was started with and how it ended, its percent and line come from
// the observing row in watch_jobs, and the same rows name the tool call the card belongs to. Counting a
// finished run's annotations would be a CVAT call of its own (`annotation_summary`, V1) — the route
// never does that: it answers what the gateway holds, and the agent's summary stays the source of the
// counts.

// annotationAnswer is the answer of GET /api/chats/{id}/annotation.
type annotationAnswer struct {
	// CVATURL: the public address of CVAT (AGW_CVAT_PUBLIC_URL), the base every link of the cards starts
	// with; empty when this setup runs no pre-annotation. The UI shows links that begin with it and no
	// others.
	CVATURL string          `json:"cvat_url"`
	Runs    []annotationRun `json:"runs"`
	Jobs    []annotationJob `json:"jobs"`
}

// annotationRun is one line of the answer: the stored run (V1), the state of its observed job and the
// tool call that started it.
type annotationRun struct {
	RunID string `json:"run_id"`
	// DatasetID and TaskID: what the run covers and where its annotations land; Function is the id of the
	// function, which is its name (V2).
	DatasetID     int64             `json:"dataset_id"`
	TaskID        int64             `json:"task_id"`
	Function      string            `json:"function"`
	MinConfidence int               `json:"min_confidence"`
	Mapping       map[string]string `json:"mapping"`
	// Existing: keep or replace; a run with replace wipes every annotation of the task and its card
	// carries the irreversible chip.
	Existing string `json:"existing"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	// Progress: the percent the watcher last wrote; nil while none was reported (queued).
	Progress *float64 `json:"progress,omitempty"`
	Message  string   `json:"message,omitempty"`
	// ToolCallID: the tool call that started the run (from watch_jobs); empty: unknown.
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	// CVATURL: the task's address in CVAT; empty without AGW_CVAT_PUBLIC_URL.
	CVATURL string `json:"cvat_url,omitempty"`
}

// annotationJob is one line of the answer: a check or a fill (V3) with the call it was started with and
// the result a finished one wrote. The result goes through whole, as the gateway stored it — the card
// shows the first rows of a list and expands them, so it needs all of them (the agent's tool
// `annotation_job` cuts its answer into pages of 50 instead, to keep its context small).
type annotationJob struct {
	JobID     string `json:"job_id"`
	Kind      string `json:"kind"` // calibrate | uncertain | fill
	DatasetID int64  `json:"dataset_id"`
	TaskID    int64  `json:"task_id"`
	Function  string `json:"function"`
	State     string `json:"state"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	// Params: the call the agent made (limit, mapping, uncertain_below, min_confidence); the card reads
	// the threshold it shows from here.
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	// ToolCallID: the tool call that started the job (from watch_jobs); empty: unknown.
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	CVATURL    string     `json:"cvat_url,omitempty"`
}

// annotationResults is GET /api/chats/{id}/annotation: the runs and jobs of a chat, each oldest first.
func (s *Server) annotationResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	runs, err := s.M.AnnotationRuns(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	jobs, err := s.M.AnnotationJobs(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	watches, err := s.M.Jobs(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	// The observing rows are matched by kind and reference (the run id, the job id); the newest
	// observation of an item is the one that carries its state, so the first match wins.
	byRef := map[string]store.WatchJob{}
	for _, j := range watches {
		if key := j.Kind + "/" + j.Ref; byRef[key].ID == "" {
			byRef[key] = j
		}
	}
	base := strings.TrimRight(s.Env.CVATPublicURL, "/")
	out := annotationAnswer{CVATURL: base, Runs: []annotationRun{}, Jobs: []annotationJob{}}
	for _, row := range runs {
		out.Runs = append(out.Runs, annotationRunLine(row, byRef[annotation.WatchKindName+"/"+row.ID], base))
	}
	for _, row := range jobs {
		line := annotationJobLine(row, byRef[annotation.JobKindName+"/"+row.ID], base)
		// The stored result carries references to tools (suggestions); the card shows them in the forms of the
		// chat's bindings, never as markers (#200).
		if len(line.Result) > 0 {
			line.Result = json.RawMessage(s.M.RenderNote(r.Context(), id, string(line.Result)))
		}
		out.Jobs = append(out.Jobs, line)
	}
	writeJSON(w, 200, out)
}

// annotationRunLine turns a stored run into the line of the route.
func annotationRunLine(row store.AnnotationRun, watch store.WatchJob, base string) annotationRun {
	line := annotationRun{
		RunID: row.ID, DatasetID: row.DatasetID, TaskID: row.TaskID, Function: row.Function,
		MinConfidence: row.MinConfidence, Mapping: map[string]string{}, Existing: row.Existing,
		State: row.State, Error: platform.RedactText(row.Error), Progress: watch.Progress, Message: watch.Message,
		ToolCallID: watch.ToolCallID, CreatedAt: row.CreatedAt, EndedAt: row.EndedAt,
	}
	if row.Mapping != "" {
		_ = json.Unmarshal([]byte(row.Mapping), &line.Mapping)
	}
	line.CVATURL = cvatTaskURL(base, row.TaskID)
	return line
}

// annotationJobLine turns a stored job into the line of the route.
func annotationJobLine(row store.AnnotationJob, watch store.WatchJob, base string) annotationJob {
	line := annotationJob{
		JobID: row.ID, Kind: row.Kind, DatasetID: row.DatasetID, TaskID: row.TaskID, Function: row.Function,
		State: row.State, Done: row.Done, Total: row.Total, Error: platform.RedactText(row.Error),
		ToolCallID: watch.ToolCallID, CreatedAt: row.CreatedAt, EndedAt: row.EndedAt,
	}
	// The stored JSON goes through unchanged; a value that cannot be read is left out rather than
	// breaking the answer of the whole route.
	if json.Valid([]byte(row.Params)) {
		line.Params = json.RawMessage(row.Params)
	}
	if res := platform.RedactText(row.Result); res != "" && json.Valid([]byte(res)) {
		line.Result = json.RawMessage(res)
	}
	line.CVATURL = cvatTaskURL(base, row.TaskID)
	return line
}

// cvatTaskURL is the link of a CVAT task for the user's browser; empty without a public address.
func cvatTaskURL(base string, taskID int64) string {
	if base == "" || taskID <= 0 {
		return ""
	}
	return base + "/tasks/" + strconv.FormatInt(taskID, 10)
}

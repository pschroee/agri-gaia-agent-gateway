// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// The states of a run as the gateway reports them.
const (
	StateRunning = "running" // no exit code written yet
	StateDone    = "done"    // exit code 0
	StateFailed  = "failed"  // any other exit code
)

// stateOf is the state that belongs to an exit code (nil: the run has not ended).
func stateOf(exit *int) string {
	switch {
	case exit == nil:
		return StateRunning
	case *exit == 0:
		return StateDone
	}
	return StateFailed
}

// RunInfo is what the gateway knows about a run: what it started (the stored row) and what the run
// directory says (start, exit code, end, results).
type RunInfo struct {
	RunID           string               `json:"run_id"`
	Entry           string               `json:"entry"`
	State           string               `json:"state"`
	ExitCode        *int                 `json:"exit_code,omitempty"`
	StartedAt       string               `json:"started_at,omitempty"`
	FinishedAt      string               `json:"finished_at,omitempty"`
	DurationSeconds int64                `json:"duration_seconds,omitempty"`
	TimeoutMinutes  int                  `json:"timeout_minutes"`
	Datasets        []int64              `json:"dataset_ids,omitempty"`
	Files           []store.NotebookFile `json:"files,omitempty"`
	Results         []RunFile            `json:"results,omitempty"`
	Link            string               `json:"link,omitempty"`
	// Note carries what the store alone cannot say, e.g. that the server is gone.
	Note string `json:"note,omitempty"`
}

// RunFile is one file in the run directory, as find reports it.
type RunFile struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// markers are the files the launcher writes into .agw/ of a run.
type markers struct {
	Started  string
	Finished string
	ExitCode *int
}

// statusScript reports the markers of a run and the files in its directory. It always ends with 0,
// so that a missing marker file is no failure of the command.
const statusScript = `cd "$1" || exit 9
for f in started finished exit_code; do
  if [ -f ".agw/$f" ]; then printf '%s\t%s\n' "$f" "$(cat ".agw/$f")"; fi
done
find . -maxdepth 3 -type f -printf 'file\t%P\t%s\n' 2>/dev/null | head -n 200
exit 0`

// logScript prints the end of a run's log; a run without a log answers with nothing.
const logScript = `f="$1/run.log"; [ -f "$f" ] || exit 0; tail -n "$2" -- "$f"`

// readRun reads the markers and the file list of a run from its directory.
func (n *Notebooks) readRun(ctx context.Context, containerID, runID string) (markers, []RunFile, error) {
	out, err := n.mustExec(ctx, containerID, []string{"bash", "-c", statusScript, "_", runDir(runID)}, nil)
	if err != nil {
		return markers{}, nil, err
	}
	return parseStatus(string(out))
}

// markers reads only the markers of a run.
func (n *Notebooks) markers(ctx context.Context, containerID, runID string) (markers, error) {
	m, _, err := n.readRun(ctx, containerID, runID)
	return m, err
}

// parseStatus reads the lines of statusScript.
func parseStatus(out string) (markers, []RunFile, error) {
	var m markers
	files := []RunFile{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, "\t")
		switch {
		case len(f) == 2 && f[0] == "started":
			m.Started = f[1]
		case len(f) == 2 && f[0] == "finished":
			m.Finished = f[1]
		case len(f) == 2 && f[0] == "exit_code":
			code, err := strconv.Atoi(strings.TrimSpace(f[1]))
			if err != nil {
				return m, nil, fmt.Errorf("the run reports an exit code of %q", f[1])
			}
			m.ExitCode = &code
		case len(f) == 3 && f[0] == "file":
			size, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil {
				size = 0
			}
			files = append(files, RunFile{Path: f[1], Bytes: size})
		}
	}
	return m, files, nil
}

// Status reads a run: the stored row (which files, which limits) and the run directory (start, exit
// code, end, results). An exit code it finds is recorded in the store, so that a run which ended
// while nobody looked still gets its end.
func (n *Notebooks) Status(ctx context.Context, chatID, runID string) (RunInfo, error) {
	row, err := n.store.GetNotebookRun(ctx, chatID, runID)
	if err != nil {
		return RunInfo{}, err
	}
	info := RunInfo{RunID: row.ID, Entry: row.Entry, State: StateRunning, TimeoutMinutes: row.TimeoutMin,
		Datasets: row.DatasetIDs, Files: row.Files, Link: n.Link(row.HubUser, row.Server, row.ID)}
	applyRun(&info, row)
	containerID, err := n.container(ctx, row.HubUser, row.Server)
	if err != nil {
		if errors.Is(err, errNoContainer) {
			info.Note = "the notebook server of this chat is not running; its directory is gone, a new run starts it again"
			return info, nil
		}
		return info, err
	}
	m, files, err := n.readRun(ctx, containerID, row.ID)
	if err != nil {
		return info, err
	}
	applyMarkers(&info, m)
	info.Results = files
	if m.ExitCode != nil && row.FinishedAt == nil {
		if err := n.store.FinishNotebookRun(ctx, row.ID, *m.ExitCode, timeOf(n.now(), m.Finished)); err != nil {
			return info, err
		}
	}
	return info, nil
}

// Log reads the end of a run's output.
func (n *Notebooks) Log(ctx context.Context, chatID, runID string, tail int) (string, error) {
	row, err := n.store.GetNotebookRun(ctx, chatID, runID)
	if err != nil {
		return "", err
	}
	if tail < 1 {
		tail = 200
	}
	if tail > platform.NotebookLogMaxTail {
		tail = platform.NotebookLogMaxTail
	}
	containerID, err := n.container(ctx, row.HubUser, row.Server)
	if err != nil {
		if errors.Is(err, errNoContainer) {
			return "", fmt.Errorf("the notebook server of this chat is not running; its directory with the log is gone")
		}
		return "", err
	}
	out, err := n.mustExec(ctx, containerID, []string{"bash", "-c", logScript, "_", runDir(runID), strconv.Itoa(tail)}, nil)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// applyRun fills in what the stored row alone says.
func applyRun(info *RunInfo, row store.NotebookRun) {
	info.StartedAt = row.StartedAt.UTC().Format(time.RFC3339)
	if row.FinishedAt != nil {
		info.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339)
		info.DurationSeconds = int64(row.FinishedAt.Sub(row.StartedAt).Seconds())
	}
	info.ExitCode = row.ExitCode
	info.State = stateOf(row.ExitCode)
	if row.FinishedAt == nil {
		info.State = StateRunning
	}
}

// applyMarkers overrules the stored state with what the run directory says (it is what the run
// itself wrote, and it survives a gateway restart).
func applyMarkers(info *RunInfo, m markers) {
	if m.Started != "" {
		info.StartedAt = m.Started
	}
	if m.Finished != "" {
		info.FinishedAt = m.Finished
	}
	if m.ExitCode != nil {
		info.ExitCode = m.ExitCode
		info.State = stateOf(m.ExitCode)
	}
	if info.StartedAt != "" && info.FinishedAt != "" {
		if from, err := time.Parse(time.RFC3339, info.StartedAt); err == nil {
			if to, err := time.Parse(time.RFC3339, info.FinishedAt); err == nil {
				info.DurationSeconds = int64(to.Sub(from).Seconds())
			}
		}
	}
}

// timeOf reads a timestamp of the run directory, or falls back to the given time.
func timeOf(fallback time.Time, stamp string) time.Time {
	if t, err := time.Parse(time.RFC3339, stamp); err == nil {
		return t
	}
	return fallback
}

// --- the answers of the tools ---

// serverStatus answers notebook_status: state, limits and the link the user can open.
func (n *Notebooks) serverStatus(ctx context.Context, chatID, user, server string) (platform.Result, error) {
	state, err := n.hub.ServerState(ctx, user, server)
	if err != nil {
		return unavailableHub(err), nil
	}
	link := ""
	if base := strings.TrimRight(n.cfg.PublicURL, "/"); base != "" {
		link = base + "/user/" + user + "/" + server + "/lab"
	}
	answer := map[string]any{
		"server": server, "state": state, "cpus": 2, "memory_gb": 8,
		"internet": false, "platform_access": false,
		"timeout_max_minutes": n.timeoutMax(), "max_files": platform.NotebookMaxFiles,
		"max_file_mb": platform.NotebookFileMaxBytes >> 20, "max_total_mb": platform.NotebookFilesMax >> 20,
		"max_datasets": platform.NotebookMaxDatasets, "link": link,
	}
	return jsonResult(answer)
}

// stopResult answers notebook_stop.
func (n *Notebooks) stopResult(ctx context.Context, chatID, user, server string) (platform.Result, error) {
	if err := n.hub.Stop(ctx, user, server); err != nil {
		return unavailableHub(err), nil
	}
	return jsonResult(map[string]any{"server": server, "state": "stopped",
		"note": "the files of earlier runs stay in the server; a new run starts it again"})
}

// runsResult answers notebook_runs: the chat's runs, newest first.
func (n *Notebooks) runsResult(ctx context.Context, chatID string) (platform.Result, error) {
	rows, err := n.store.NotebookRuns(ctx, chatID)
	if err != nil {
		return errorResult("the runs of this chat are not readable: %v", err)
	}
	type rowView struct {
		RunID           string `json:"run_id"`
		Entry           string `json:"entry"`
		State           string `json:"state"`
		ExitCode        *int   `json:"exit_code,omitempty"`
		StartedAt       string `json:"started_at"`
		FinishedAt      string `json:"finished_at,omitempty"`
		DurationSeconds int64  `json:"duration_seconds,omitempty"`
		TimeoutMinutes  int    `json:"timeout_minutes"`
		Link            string `json:"link,omitempty"`
	}
	out := make([]rowView, 0, len(rows))
	for _, row := range rows {
		if row.FinishedAt == nil {
			// A run that ended while nobody looked still gets its end (the watcher and the status
			// call do the same).
			if containerID, cerr := n.container(ctx, row.HubUser, row.Server); cerr == nil {
				_ = n.settleFinished(ctx, containerID, row)
				if fresh, gerr := n.store.GetNotebookRun(ctx, chatID, row.ID); gerr == nil {
					row = fresh
				}
			}
		}
		v := rowView{RunID: row.ID, Entry: row.Entry, State: stateOf(row.ExitCode), ExitCode: row.ExitCode,
			StartedAt: row.StartedAt.UTC().Format(time.RFC3339), TimeoutMinutes: row.TimeoutMin,
			Link: n.Link(row.HubUser, row.Server, row.ID)}
		if row.FinishedAt != nil {
			v.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339)
			v.DurationSeconds = int64(row.FinishedAt.Sub(row.StartedAt).Seconds())
		} else {
			v.State = StateRunning
		}
		out = append(out, v)
	}
	return jsonResult(map[string]any{"runs": out})
}

// statusResult answers notebook_run_status.
func (n *Notebooks) statusResult(ctx context.Context, chatID, runID string) (platform.Result, error) {
	info, err := n.Status(ctx, chatID, runID)
	if errors.Is(err, store.ErrNotFound) {
		return errorResult("%s is not a run of this chat; notebook_runs lists them", runID)
	}
	if err != nil {
		return errorResult("the state of %s is not readable: %v", runID, err)
	}
	return jsonResult(info)
}

// logResult answers notebook_log.
func (n *Notebooks) logResult(ctx context.Context, chatID, runID string, req platform.Request) (platform.Result, error) {
	tail := 200
	if v := req.Query["tail"]; v != "" {
		lines, err := strconv.Atoi(v)
		if err != nil || lines < 1 {
			return errorResult("tail must be a positive number of lines")
		}
		tail = lines
	}
	log, err := n.Log(ctx, chatID, runID, tail)
	if errors.Is(err, store.ErrNotFound) {
		return errorResult("%s is not a run of this chat; notebook_runs lists them", runID)
	}
	if err != nil {
		return errorResult("the log of %s is not readable: %v", runID, err)
	}
	return platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: log}, nil
}

// timeoutMax is the ceiling of a run in this setup.
func (n *Notebooks) timeoutMax() int {
	if n.cfg.TimeoutMaxMin > 0 {
		return n.cfg.TimeoutMaxMin
	}
	return platform.NotebookTimeoutMax
}

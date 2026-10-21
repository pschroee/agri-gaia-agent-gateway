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
	"agw/internal/watch"
)

// The states of a run as the gateway reports them.
const (
	StateRunning = "running" // no exit code written yet
	StateDone    = "done"    // exit code 0
	StateFailed  = "failed"  // any other exit code
)

// The two notes a status answer carries when the chat's notebook server is not running (own code C-E4).
const (
	// serverGoneNote: the run ended before the server went away, only its directory is gone.
	serverGoneNote = "the notebook server of this chat is not running; its directory is gone, a new run starts it again"
	// stoppedRunNote: the server went away while the run was going — it was stopped, and the run ended
	// without an exit code. The card shows the run as failed with this line (a stop is no exit code of
	// the code itself, and the gateway states no code it did not read).
	stoppedRunNote = "the run ended without an exit code: the notebook server of this chat was stopped while it went"
)

// stateOf is the state that belongs to a stored run: running while it has no end, done after exit code 0,
// failed after any other exit code — and failed for a run that ended without a code at all, because its
// notebook server was stopped while it went (own code C-E4; no code the gateway has not read).
func stateOf(exit *int, finished bool) string {
	switch {
	case !finished:
		return StateRunning
	case exit != nil && *exit == 0:
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
	// Notebook is the executed notebook of a .ipynb run in the run directory (it exists from the start and
	// grows with every cell); NotebookCopy its copy in the user's notebooks directory, once the run has ended.
	Notebook     string `json:"notebook,omitempty"`
	NotebookCopy string `json:"notebook_copy,omitempty"`
	// FailedCell is the number of the cell that failed or was running at the time limit (the "# cell n" of the
	// approval), CellError what went wrong there. Written by the run's own uid, so it only informs.
	FailedCell *int   `json:"failed_cell,omitempty"`
	CellError  string `json:"cell_error,omitempty"`
	// GPU is the device the run saw (nvidia-smi -L in the run), "none" when there was none; empty
	// for runs that did not record it. Written by the run's own uid, so it only informs.
	GPU string `json:"gpu,omitempty"`
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
	GPU      *string // nil: the run recorded nothing
	// FailedCell and CellError: the launcher's note on the cell of a notebook run that failed (0: none).
	FailedCell int
	CellError  string
}

// statusScript reports the markers of a run and the files in its directory. It always ends with 0,
// so that a missing marker file is no failure of the command.
const statusScript = `cd "$1" || exit 9
for f in started finished exit_code gpu failed_cell; do
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

// gpuName reads the name of the GPU from a line of nvidia-smi -L ("GPU 0: Name (UUID: GPU-...)"); no
// line means "none". The text comes from the run, so controls are made visible and the length is bounded.
func gpuName(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return "none"
	}
	if i := strings.Index(line, ": "); i >= 0 {
		line = line[i+2:]
	}
	if i := strings.Index(line, " (UUID"); i >= 0 {
		line = line[:i]
	}
	line = platform.VisibleControls(strings.TrimSpace(line))
	if r := []rune(line); len(r) > 120 {
		line = string(r[:120])
	}
	if line == "" {
		return "none"
	}
	return line
}

// gpuMismatchNote is what the status says when the instance assigns a GPU but the run saw none.
const gpuMismatchNote = "this instance assigns a GPU to notebook servers, but this run saw none (it ran on the CPU); " +
	"the server may have started before the GPU was assigned - stop it with notebook-stop and run again"

// cellErrorText makes the error name of a cell visible and bounds it (120 runes): it comes from the run.
func cellErrorText(s string) string {
	s = platform.VisibleControls(strings.TrimSpace(s))
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	return s
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
		case len(f) == 2 && f[0] == "gpu":
			g := gpuName(f[1])
			m.GPU = &g
		case len(f) >= 2 && f[0] == "failed_cell":
			// The note is "<n><TAB>text".
			if n, err := strconv.Atoi(strings.TrimSpace(f[1])); err == nil && n > 0 {
				m.FailedCell = n
				m.CellError = cellErrorText(strings.Join(f[2:], "\t"))
			}
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
		Datasets: row.DatasetIDs, Files: row.Files, Link: n.Link(row.HubUser, row.Server, row.ID, row.Entry)}
	applyRun(&info, row)
	containerID, err := n.container(ctx, row.HubUser, row.Server)
	if err != nil {
		if errors.Is(err, errNoContainer) {
			// The server of this chat is gone and with it whatever ran inside it: the code of a run that
			// has not ended cannot go on, and without an end the run would block the next one
			// (ActiveNotebookRun) and keep its job "running" for good. Its end is recorded here, without
			// an exit code (own code C-E4: the user's stop of a run ends the server, see the kind's Stop).
			if row.FinishedAt == nil {
				at := n.now()
				ok, err := n.store.EndNotebookRun(ctx, row.ID, at)
				if err != nil {
					return RunInfo{}, err
				}
				if ok {
					row.FinishedAt = &at
					applyRun(&info, row)
					info.Note = stoppedRunNote
				}
			}
			if info.Note == "" {
				info.Note = serverGoneNote
			}
			return info, nil
		}
		return info, err
	}
	m, files, err := n.readRun(ctx, containerID, row.ID)
	if err != nil {
		return info, err
	}
	applyMarkers(&info, m)
	if n.Shape().GPU && info.GPU == "none" && info.Note == "" {
		info.Note = gpuMismatchNote
	}
	info.Results = files
	if m.ExitCode != nil && row.FinishedAt == nil {
		if err := n.store.FinishNotebookRun(ctx, row.ID, *m.ExitCode, timeOf(n.now(), m.Finished)); err != nil {
			return info, err
		}
	}
	return info, nil
}

// Log reads the end of a run's output with progress bars collapsed (issue #178), so it can hold fewer
// lines than asked for.
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
	return watch.CollapseProgress(string(out)), nil
}

// applyRun fills in what the stored row alone says.
func applyRun(info *RunInfo, row store.NotebookRun) {
	info.StartedAt = row.StartedAt.UTC().Format(time.RFC3339)
	if row.FinishedAt != nil {
		info.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339)
		info.DurationSeconds = int64(row.FinishedAt.Sub(row.StartedAt).Seconds())
	}
	info.ExitCode = row.ExitCode
	info.State = stateOf(row.ExitCode, row.FinishedAt != nil)
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
		info.State = stateOf(m.ExitCode, true) // the run directory wrote an exit code: the run has ended
	}
	if m.GPU != nil {
		info.GPU = *m.GPU
	}
	if strings.HasSuffix(info.Entry, ".ipynb") {
		info.Notebook = executedName(info.Entry)
		if m.ExitCode != nil {
			info.NotebookCopy = "work/" + workspaceCopy(info.RunID, info.Entry)
		}
		if m.FailedCell > 0 {
			c := m.FailedCell
			info.FailedCell, info.CellError = &c, m.CellError
		}
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
		"server": server, "state": state, "cpus": n.Shape().CPUs, "memory_gb": n.Shape().MemGB, "gpu": n.Shape().GPU,
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
	// Who stopped it is recorded where the caller is known (own code C-E4): the user's own stop route marks the
	// job in Manager.StopJob, the agent's own call in Manager.PlatformCall — this call cannot tell the two apart
	// (the invocation is set for the tools with steps, the tool call id is a header the client sets).
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
		v := rowView{RunID: row.ID, Entry: row.Entry, State: stateOf(row.ExitCode, row.FinishedAt != nil), ExitCode: row.ExitCode,
			StartedAt: row.StartedAt.UTC().Format(time.RFC3339), TimeoutMinutes: row.TimeoutMin,
			Link: n.Link(row.HubUser, row.Server, row.ID, row.Entry)}
		if row.FinishedAt != nil {
			v.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339)
			v.DurationSeconds = int64(row.FinishedAt.Sub(row.StartedAt).Seconds())
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

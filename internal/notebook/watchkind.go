// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/watch"
)

// The observer kind of a run (G1.7, internal/watch): the watcher of the autonomy plan calls Poll and
// gets the state of a run out of the run directory. Nothing here polls on its own — the agent asks
// notebook_run_status for that, and both paths record the end of the run in the store.

// KindName is the name of the kind.
const KindName = "notebook_run"

// RunRoute is the call that starts a run: method and path pattern for watch.RegisterRoute.
func RunRoute() (string, *regexp.Regexp) {
	return http.MethodPost, regexp.MustCompile("^" + regexp.QuoteMeta(platform.NotebookPrefix) + "/runs$")
}

// RunIDFrom reads the reference (the run id) out of the answer of a run.
func RunIDFrom(_ platform.Request, res platform.Result) (string, bool) {
	var doc struct {
		RunID string `json:"run_id"`
	}
	if res.Status != "ok" || json.Unmarshal([]byte(res.Body), &doc) != nil || doc.RunID == "" {
		return "", false
	}
	return doc.RunID, true
}

// WatchKind is the kind of job that observes one run of this chat's notebook server.
func (n *Notebooks) WatchKind() watch.Kind {
	return watch.Kind{Name: KindName, Resource: delegation.Notebook, Poll: n.poll, Stop: stopRun}
}

// StoppingCall reports whether a call stops this chat's notebook server (own code C-E4): the run that was going
// in it ends with the server, and the job of that run then names who stopped it (watch_jobs.stopped_by). The
// caller knows whether the agent or the user asked — the call itself does not.
func StoppingCall(req platform.Request) bool {
	return req.Method == http.MethodPost && req.Path == platform.NotebookPrefix+"/stop"
}

// stopRun builds the call that ends a run of this kind at the user's request ("Stop job", A-E2 D2.9, own
// code C-E4). A single run is not addressable: the code runs in the chat's notebook server, so the call
// stops that server — the hub removes its container (Hub.Stop: DELETE /users/<user>/servers/<server>),
// and with the container the code that runs in it. The run then ends without an exit code, and Status
// records and explains that. Without a run id in the reference there is nothing to stop, so the kind
// refuses as the other kinds do.
func stopRun(j watch.Job) (platform.Request, bool) {
	if !platform.NotebookRunIDRe.MatchString(j.Ref) {
		return platform.Request{}, false
	}
	return platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/stop"}, true
}

// poll answers the state of a run. It reads the same way a status call does, so a run that ended
// while nobody looked gets its end recorded here too. The link goes along (own code C-E4): it is the
// address of the run the card offers, and the gateway is the one that knows it.
func (n *Notebooks) poll(ctx context.Context, j watch.Job, _ watch.Reader) (watch.Progress, error) {
	info, err := n.Status(ctx, j.ChatID, j.Ref)
	if err != nil {
		return watch.Progress{}, err
	}
	p := watch.Progress{State: info.State, Message: runMessage(info), Outputs: runOutputs(info), Link: info.Link}
	if info.State != StateRunning {
		pct := 100.0
		p.Percent = &pct
	} else if info.TimeoutMinutes > 0 && info.StartedAt != "" {
		if started, err := time.Parse(time.RFC3339, info.StartedAt); err == nil {
			pct := time.Since(started).Seconds() / (float64(info.TimeoutMinutes) * 60) * 100
			if pct > 99 {
				pct = 99
			}
			p.Percent = &pct
		}
	}
	if log, err := n.Log(ctx, j.ChatID, j.Ref, 20); err == nil {
		p.LogTail = log
	}
	if m := n.runMetrics(ctx, j.ChatID, j.Ref); m != nil {
		p.Metrics = m
		if info.State == StateRunning {
			// The epochs a run names are a better measure than the share of its time limit (issue #168).
			if pct := watch.MetricsPercent(m, false); pct != nil {
				p.Percent = pct
			}
		}
	}
	return p, nil
}

// The files a run may write for the training search (issue #168): the figures of every epoch and, when the
// launcher found a GPU, the samples of nvidia-smi. Both are read from the run directory with realpath, as Fetch
// does, so that a symbolic link out of the directory reads nothing; the output is bounded in bytes and lines.
const (
	metricsFile = "outputs/metrics.jsonl"
	gpuFile     = ".agw/gpu.csv"
	paramsFile  = "params.json"
	// maxParamsBytes is the size of params.json the gateway reads (the trial parameters are at most 4 KiB).
	maxParamsBytes = 4 << 10
)

// tailScript prints the last lines of a file of a run: $1 the run directory, $2 the relative path, $3 the number
// of lines, $4 the number of bytes read at most. A missing file, a link out of the directory and a directory give
// no output and no error.
const tailScript = `f=$(realpath -e -- "$1/$2" 2>/dev/null) || exit 0
case "$f" in "$1"/*) ;; *) exit 0;; esac
[ -f "$f" ] || exit 0
tail -c "$4" -- "$f" | tail -n "$3"`

// headScript prints the beginning of a file of a run: $1 the run directory, $2 the relative path, $3 the bytes.
const headScript = `f=$(realpath -e -- "$1/$2" 2>/dev/null) || exit 0
case "$f" in "$1"/*) ;; *) exit 0;; esac
[ -f "$f" ] || exit 0
head -c "$3" -- "$f"`

// runMetrics reads the figures of a run out of its directory: outputs/metrics.jsonl, .agw/gpu.csv and params.json.
// It returns nil when the server is not running or the run left nothing; a failure is never fatal, the exit code
// decides about the run.
func (n *Notebooks) runMetrics(ctx context.Context, chatID, runID string) *watch.Metrics {
	row, err := n.store.GetNotebookRun(ctx, chatID, runID)
	if err != nil {
		return nil
	}
	containerID, err := n.container(ctx, row.HubUser, row.Server)
	if err != nil {
		return nil
	}
	dir := runDir(runID)
	read := func(script string, args ...string) []byte {
		cmd := append([]string{"bash", "-c", script, "_", dir}, args...)
		out, code, _ := n.exec(ctx, containerID, cmd, nil)
		if code != 0 {
			return nil
		}
		return out
	}
	m := &watch.Metrics{Supported: true}
	m.Epochs = search.ParseMetricsJSONL(read(tailScript, metricsFile, strconv.Itoa(search.MaxMetricsLines),
		strconv.Itoa(search.MaxMetricsLines*search.MaxMetricsLine)))
	if raw := read(tailScript, gpuFile, strconv.Itoa(search.MaxGPULines), "262144"); len(raw) > 0 {
		m.GPU = search.ParseGPUCSV(raw)
		m.GPUAvailable = true
	}
	m.Params = canonicalParams(read(headScript, paramsFile, strconv.Itoa(maxParamsBytes+1)))
	if len(m.Epochs) == 0 && len(m.GPU) == 0 && m.Params == nil {
		return nil
	}
	return m
}

// canonicalParams returns params.json as canonical JSON when it is an object of at most maxParamsBytes; anything
// else (too long, not JSON, duplicate keys, not an object) is none.
func canonicalParams(raw []byte) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxParamsBytes || raw[0] != '{' {
		return nil
	}
	canon, err := plan.Canonical(raw)
	if err != nil {
		return nil
	}
	return canon
}

// runMessage is the one line the watcher shows for a run (at most 200 runes).
func runMessage(info RunInfo) string {
	switch {
	case info.ExitCode != nil && info.DurationSeconds > 0:
		return fmt.Sprintf("exit %d after %s", *info.ExitCode, durationText(info.DurationSeconds))
	case info.ExitCode != nil:
		return fmt.Sprintf("exit %d", *info.ExitCode)
	case info.Note != "":
		return info.Note
	case info.TimeoutMinutes > 0:
		return fmt.Sprintf("running, at most %d minutes", info.TimeoutMinutes)
	}
	return "running"
}

// durationText writes a duration for the one line of a job.
func durationText(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%d s", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%d min %d s", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d h %d min", seconds/3600, (seconds%3600)/60)
}

// runOutputs names the files a run left in its directory as its results.
func runOutputs(info RunInfo) []watch.Output {
	out := make([]watch.Output, 0, len(info.Results))
	for _, f := range info.Results {
		out = append(out, watch.Output{Resource: "notebook_file", ID: info.RunID + "/" + f.Path, Label: f.Path,
			Size: f.Bytes})
	}
	return out
}

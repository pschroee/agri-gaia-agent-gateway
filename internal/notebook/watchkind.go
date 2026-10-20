// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
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
	return p, nil
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

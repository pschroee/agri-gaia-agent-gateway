// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// A run: the gateway starts the chat's server, puts the approved files into a fresh run directory,
// starts them there and records the run. What runs is exactly the bytes the user approved: they come
// from the socket's readFiles (SHA-256 in the approval), not from the sandbox at start time.
//
// The exit code and the log live in the run directory inside the container (.agw/), written by the
// launcher. The run survives a restart of the gateway; the watcher kind notebook_run (watchkind.go)
// carries its end into the store. The code runs with the same uid as those files, so it could fake
// them — that only affects what the agent is told, never a decision of the gateway (plan 1.5).

// runsDir is where the run directories live in the notebook user's home.
const runsDir = workDir + "/runs"

// datasetScript unpacks a dataset archive into the notebook user's home. It is written to be
// idempotent: an unpacked dataset carries .agw-complete and is not fetched again.
const datasetScript = `set -e
d="/home/jovyan/work/datasets/$1"
[ -f "$d/.agw-complete" ] && { cat > /dev/null; exit 0; }
rm -rf "$d"; mkdir -p "$d"
cat > "$d/.dataset.zip"
cd "$d" && python -m zipfile -e .dataset.zip . && rm .dataset.zip && touch .agw-complete`

// runScript is the launcher the gateway writes into the run directory. entry, args and the run id
// are checked against fixed patterns before; they are single-quoted anyway.
func runScript(runID, entry string, args []string, timeoutMin int) string {
	q := func(s string) string { return "'" + s + "'" } // the patterns exclude the single quote
	cmd := "python -u " + q(entry)
	if strings.HasSuffix(entry, ".ipynb") {
		cmd = "jupyter nbconvert --to notebook --execute --ExecutePreprocessor.timeout=-1 --output " +
			q("executed-"+entry) + " " + q(entry)
	}
	for _, a := range args {
		cmd += " " + q(a)
	}
	return "#!/bin/bash\n# written by the agent gateway\n" +
		"cd " + q(runsDir+"/"+runID) + " || exit 99\n" +
		"date -u +%FT%TZ > .agw/started\n" +
		"nvidia-smi -L 2>/dev/null | head -n 1 > .agw/gpu\n" +
		// The GPU is sampled every 5 s into .agw/gpu.csv while the run goes (issue #168), only when there is one.
		"if [ -s .agw/gpu ]; then\n" +
		"  nvidia-smi --query-gpu=timestamp,utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits -l 5 >> .agw/gpu.csv 2>/dev/null &\n" +
		"  SMI=$!\n" +
		"fi\n" +
		fmt.Sprintf("timeout --signal=TERM --kill-after=30s %dm %s > run.log 2>&1\n", timeoutMin, cmd) +
		"echo $? > .agw/exit_code\n" +
		"[ -n \"$SMI\" ] && kill \"$SMI\" 2>/dev/null\n" +
		"date -u +%FT%TZ > .agw/finished\n"
}

// newRunID names a run: the time it starts and four random hex digits, so that two runs of the same
// second stay apart (and a directory of an earlier run is never reused).
func newRunID(now time.Time) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		copy(b[:], []byte{0x00, 0x01})
	}
	return "r" + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(b[:])
}

// runDir is the directory of a run inside the container.
func runDir(runID string) string { return runsDir + "/" + runID }

// Run starts one run of the chat: it starts the server, unpacks the datasets, copies the files in
// and starts the launcher. datasets reads one platform dataset archive (the caller streams it, so
// the gateway holds no copy of it).
func (n *Notebooks) Run(ctx context.Context, chatID, user string, body platform.RunBody, files []platform.Upload,
	datasets func(ctx context.Context, id int64) (io.ReadCloser, error)) (store.NotebookRun, error) {
	names := make([]string, 0, len(files))
	sizes := make([]int64, 0, len(files))
	approved := make([]store.NotebookFile, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
		sizes = append(sizes, int64(len(f.Data)))
		approved = append(approved, store.NotebookFile{Name: f.Name, Size: int64(len(f.Data)), SHA256: f.SHA256})
	}
	if err := platform.ValidateRunFiles(body, names, sizes); err != nil {
		return store.NotebookRun{}, err
	}
	server := n.Server(chatID)
	// The lock covers the time in which this run has no row yet, so the reaper cannot see the chat as
	// idle and stop the server under the starting run.
	mu := n.chatLock(chatID)
	mu.Lock()
	defer mu.Unlock()
	if err := n.startServer(ctx, user, server); err != nil {
		return store.NotebookRun{}, err
	}
	if err := n.waitReady(ctx, user, server); err != nil {
		return store.NotebookRun{}, err
	}
	id, err := n.container(ctx, user, server)
	if err != nil {
		return store.NotebookRun{}, err
	}
	if active, busy, err := n.store.ActiveNotebookRun(ctx, chatID); err != nil {
		return store.NotebookRun{}, err
	} else if busy {
		if err := n.settleFinished(ctx, id, active); err != nil {
			return store.NotebookRun{}, err
		}
		// settleFinished records the end if the directory has an exit code; only a run that really
		// still runs blocks the next one.
		if again, still, err := n.store.ActiveNotebookRun(ctx, chatID); err != nil {
			return store.NotebookRun{}, err
		} else if still {
			return store.NotebookRun{}, fmt.Errorf("run %s is still going (notebook_run_status shows it); one run at a time per chat", again.ID)
		}
	}
	for _, dsID := range body.DatasetIDs {
		if ok, err := n.exists(ctx, id, workDir+"/datasets/"+strconv.FormatInt(dsID, 10)+"/.agw-complete"); err != nil {
			return store.NotebookRun{}, err
		} else if ok {
			continue
		}
		rc, err := datasets(ctx, dsID)
		if err != nil {
			return store.NotebookRun{}, err
		}
		err = n.datasetLoad(ctx, id, dsID, rc)
		rc.Close()
		if err != nil {
			return store.NotebookRun{}, err
		}
	}
	runID := newRunID(n.now())
	archive, err := runTar(runID, body.Entry, body.Args, body.TimeoutMinutes, files)
	if err != nil {
		return store.NotebookRun{}, err
	}
	// The directory of the runs is created here: /home/jovyan/work is a volume per server, so on the
	// first run of a fresh notebook server it holds nothing but the datasets, and tar -C would fail.
	if _, err := n.mustExec(ctx, id, []string{"mkdir", "-p", runsDir}, nil); err != nil {
		return store.NotebookRun{}, err
	}
	if _, err := n.mustExec(ctx, id, []string{"tar", "-x", "-C", runsDir}, bytes.NewReader(archive)); err != nil {
		return store.NotebookRun{}, err
	}
	if _, err := n.mustExec(ctx, id, []string{"setsid", "--fork", "bash", runDir(runID) + "/.agw/run.sh"}, nil); err != nil {
		return store.NotebookRun{}, err
	}
	run := store.NotebookRun{ID: runID, ChatID: chatID, HubUser: user, Server: server, Entry: body.Entry,
		Files: approved, DatasetIDs: body.DatasetIDs, TimeoutMin: body.TimeoutMinutes, StartedAt: n.now()}
	if err := n.store.CreateNotebookRun(ctx, run); err != nil {
		return store.NotebookRun{}, err
	}
	return run, nil
}

// datasetLoad writes one dataset archive into the container: the archive comes as a stream, so the
// gateway never holds it in memory. The unpacking happens in the container (python -m zipfile); a
// hostile archive could only reach the container itself.
func (n *Notebooks) datasetLoad(ctx context.Context, containerID string, dsID int64, body io.Reader) error {
	_, err := n.mustExec(ctx, containerID, []string{"bash", "-c", datasetScript, "_", strconv.FormatInt(dsID, 10)}, body)
	return err
}

// datasetReader returns the reader of one platform dataset archive for the runner: the gateway
// streams it into the container and keeps no copy of it. Every transfer is one entry in the log (the
// agent's own call is logged by the socket; this extra call is not).
func (n *Notebooks) datasetReader(chatID string) func(context.Context, int64) (io.ReadCloser, error) {
	return func(ctx context.Context, id int64) (io.ReadCloser, error) {
		if n.plat == nil {
			return nil, platform.ErrNotConfigured
		}
		r := platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10) + "/download"}
		body, size, err := n.plat.Stream(ctx, chatID, r)
		if err != nil {
			n.log(ctx, chatID, r.String(), "error: "+err.Error())
			return nil, fmt.Errorf("dataset %d: %w", id, err)
		}
		result := "ok, the platform names no size"
		if size >= 0 {
			result = fmt.Sprintf("ok, %d bytes", size)
		}
		n.log(ctx, chatID, r.String(), result)
		return body, nil
	}
}

// runResult answers notebook_run.
func (n *Notebooks) runResult(ctx context.Context, chatID, user, server string, req platform.Request) (platform.Result, error) {
	body, err := platform.ParseRunForm(req.Form, n.timeoutMax())
	if err != nil {
		return errorResult("this run cannot start: %v", err)
	}
	run, err := n.Run(ctx, chatID, user, body, req.Uploads, n.datasetReader(chatID))
	if err != nil {
		if errors.Is(err, ErrHub) {
			return unavailableHub(err), nil
		}
		return errorResult("the run did not start: %v", err)
	}
	info := RunInfo{RunID: run.ID, Entry: run.Entry, State: StateRunning, TimeoutMinutes: run.TimeoutMin,
		Datasets: run.DatasetIDs, Files: run.Files, StartedAt: run.StartedAt.UTC().Format(time.RFC3339),
		Link: n.Link(user, server, run.ID)}
	return jsonResult(info)
}

// How long the gateway waits for a started server to become ready. Variables, so that tests do not
// wait for a real hub: the notebook image takes about half a minute to start.
var (
	serverReadyTimeout = 120 * time.Second
	serverReadyEvery   = 2 * time.Second
	// serverStopWaitTimeout: how long a run waits for a server that the hub is still stopping (the
	// hub's slow stop timeout is 10 s, the container stop up to 10 s more).
	serverStopWaitTimeout = 60 * time.Second
)

// startServer starts the server; while the hub is still stopping it, it tries again every
// serverReadyEvery, up to serverStopWaitTimeout.
func (n *Notebooks) startServer(ctx context.Context, user, server string) error {
	deadline := time.Now().Add(serverStopWaitTimeout)
	for {
		err := n.hub.Start(ctx, user, server)
		if !errors.Is(err, ErrStopPending) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the notebook server %s is still stopping after %s; try the run again in a minute", server, serverStopWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serverReadyEvery):
		}
	}
}

// waitReady waits until the hub reports the started server as ready.
func (n *Notebooks) waitReady(ctx context.Context, user, server string) error {
	deadline := time.Now().Add(serverReadyTimeout)
	for {
		ready, err := n.hub.Ready(ctx, user, server)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the notebook server %s did not become ready within %s", server, serverReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serverReadyEvery):
		}
	}
}

// settleFinished records the end of a run whose directory already holds an exit code (the run ended
// while nobody looked, e.g. while the gateway was down).
func (n *Notebooks) settleFinished(ctx context.Context, containerID string, run store.NotebookRun) error {
	markers, err := n.markers(ctx, containerID, run.ID)
	if err != nil {
		return err
	}
	if markers.ExitCode == nil {
		return nil
	}
	at := n.now()
	if markers.Finished != "" {
		if t, err := time.Parse(time.RFC3339, markers.Finished); err == nil {
			at = t
		}
	}
	return n.store.FinishNotebookRun(ctx, run.ID, *markers.ExitCode, at)
}

// runTar packs a run: the run directory, .agw/ and the approved bytes plus the launcher. The
// archive goes through the container's stdin, so nothing of it touches the gateway's disk.
func runTar(runID, entry string, args []string, timeoutMin int, files []platform.Upload) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 100, ModTime: now})
	}
	file := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data)), Uid: 1000, Gid: 100, ModTime: now}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	for _, d := range []string{runID + "/", runID + "/.agw/"} {
		if err := dir(d); err != nil {
			return nil, err
		}
	}
	if err := file(runID+"/.agw/run.sh", 0o755, []byte(runScript(runID, entry, args, timeoutMin))); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := file(runID+"/"+f.Name, 0o644, f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"agw/internal/execproto"
	"agw/internal/platform"
	"agw/internal/store"
)

// Fetch reads one result file out of a run directory. The path is checked twice: the tool refuses
// anything that is not a plain relative path (platform.CheckNotebookFilePath), and the script
// resolves it with realpath and insists that the result lies below the run directory — a symlink out
// of it is refused, whatever it points at.
const fetchScript = `f=$(realpath -e -- "$1") || exit 3
case "$f" in "$2"/*) ;; *) exit 4;; esac
[ -f "$f" ] || exit 5
head -c "$3" -- "$f"`

// Fetch returns the name and the bytes of a file of a run (at most execproto.MaxFileBytes, the
// write limit of the sandbox the file goes to).
func (n *Notebooks) Fetch(ctx context.Context, chatID, runID, path string) (string, []byte, error) {
	name, data, _, err := n.fetch(ctx, chatID, runID, path)
	return name, data, err
}

// modelMetaFile is the file a training script writes next to its model to say how it feeds an image to it
// (#181); maxModelMetaBytes is the size of it the gateway reads.
const (
	modelMetaFile     = "outputs/model.json"
	maxModelMetaBytes = 4 << 10
)

// fetch is Fetch plus a note for the agent. When the file is an ONNX model, outputs/model.json of the same
// run is read, validated with the rules of the check call and stored with the fetch (NT5).
func (n *Notebooks) fetch(ctx context.Context, chatID, runID, path string) (string, []byte, string, error) {
	if err := platform.CheckNotebookFilePath(path); err != nil {
		return "", nil, "", err
	}
	row, err := n.store.GetNotebookRun(ctx, chatID, runID)
	if err != nil {
		return "", nil, "", err
	}
	containerID, err := n.container(ctx, row.HubUser, row.Server)
	if err != nil {
		if errors.Is(err, errNoContainer) {
			return "", nil, "", fmt.Errorf("the notebook server of this chat is not running; its files stay and can be read after the next run starts the server")
		}
		return "", nil, "", err
	}
	dir := runDir(runID)
	out, code, err := n.exec(ctx, containerID,
		[]string{"bash", "-c", fetchScript, "_", dir + "/" + path, dir, strconv.Itoa(execproto.MaxFileBytes + 1)}, nil)
	if code != 0 {
		switch code {
		case 3:
			return "", nil, "", fmt.Errorf("%s is not in run %s; notebook_run_status lists the files of the run", path, runID)
		case 4:
			return "", nil, "", fmt.Errorf("%s does not lie in the run directory (a symbolic link out of it is refused)", path)
		case 5:
			return "", nil, "", fmt.Errorf("%s is not a regular file (directories cannot be fetched)", path)
		}
		return "", nil, "", fmt.Errorf("reading %s of run %s: %v", path, runID, err)
	}
	if len(out) > execproto.MaxFileBytes {
		return "", nil, "", fmt.Errorf("%s is larger than %d MB", path, execproto.MaxFileBytes>>20)
	}
	// R2.1: the gateway remembers the file it handed out, with the SHA-256 of the bytes it read. A check
	// of a file of the sandbox is measured against this row; only a file of a run of this chat has an
	// origin the gateway saw (P2). A store that fails fails the fetch — without the row the file would
	// look like one the agent brought along itself.
	var meta json.RawMessage
	note := ""
	if strings.HasSuffix(strings.ToLower(path), ".onnx") {
		raw, _, _ := n.exec(ctx, containerID,
			[]string{"bash", "-c", headScript, "_", dir, modelMetaFile, strconv.Itoa(maxModelMetaBytes + 1)}, nil)
		if len(raw) > 0 {
			if len(raw) > maxModelMetaBytes {
				note = modelMetaNote(fmt.Errorf("larger than %d bytes", maxModelMetaBytes))
			} else if checked, err := platform.CheckModelMeta(raw); err != nil {
				slog.Warn("notebook: outputs/model.json is not valid", "run", runID, "err", err)
				note = modelMetaNote(err)
			} else {
				meta = checked
			}
		}
	}
	sum := sha256.Sum256(out)
	if err := n.store.RecordNotebookFetch(ctx, store.NotebookFetch{ChatID: chatID, RunID: runID,
		Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(out)), ModelMeta: meta}); err != nil {
		return "", nil, "", fmt.Errorf("remembering the fetched file: %w", err)
	}
	return baseName(path), out, note, nil
}

// modelMetaNote is what the agent reads when outputs/model.json of the run cannot be used.
func modelMetaNote(err error) string {
	return "outputs/model.json of this run is not valid: " + err.Error() + "; the check will use the declared preprocessing"
}

// fetchResult answers notebook_fetch: the file does not go into the answer but into the execution
// sandbox (the socket writes it and replaces the body with a note).
func (n *Notebooks) fetchResult(ctx context.Context, chatID, runID string, req platform.Request) (platform.Result, error) {
	path := req.Query["path"]
	if path == "" {
		return errorResult("path missing: name the file inside the run directory")
	}
	name, data, note, err := n.fetch(ctx, chatID, runID, path)
	if errors.Is(err, store.ErrNotFound) {
		return errorResult("%s is not a run of this chat; notebook_runs lists them", runID)
	}
	if err != nil {
		return errorResult("%v", err)
	}
	return platform.Result{Status: "ok", HTTPStatus: http.StatusOK,
		Note: note, File: &platform.FileData{Dir: runID, Name: name, Data: data}}, nil
}

// baseName is the last segment of a path (the file name the fetched file gets in the workspace).
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

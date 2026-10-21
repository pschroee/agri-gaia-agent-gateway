// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"agw/internal/platform"
)

// Issue #193: a .ipynb entry runs with the launcher, takes no arguments, and the executed notebook is copied
// under its speaking name before the exit code is written.
func TestRunScriptNotebookLauncher(t *testing.T) {
	script := runScript("r20261021143005-ab12", "gpu_check.ipynb", []string{"--ignored"}, 30)
	for _, w := range []string{
		"python -u .agw/agw_run_notebook.py 'gpu_check.ipynb' --out 'executed-gpu_check.ipynb' --markers .agw > run.log 2>&1",
		"rc=$?",
		"cp -n -- 'executed-gpu_check.ipynb' '/home/jovyan/work/notebooks/gpu_check_2026-10-21_14-30-05.ipynb'",
		"echo $rc > .agw/exit_code",
	} {
		if !strings.Contains(script, w) {
			t.Errorf("script without %q:\n%s", w, script)
		}
	}
	if strings.Contains(script, "--ignored") {
		t.Errorf("a notebook takes no arguments:\n%s", script)
	}
	if strings.Index(script, "cp -n") > strings.Index(script, "echo $rc") {
		t.Error("the copy must be made before the exit code is written")
	}
	if py := runScript("r20261021143005-ab12", "train.py", nil, 30); strings.Contains(py, "notebooks/") {
		t.Errorf("a .py run makes no notebook copy:\n%s", py)
	}
}

// The launcher travels with the run: .agw/agw_run_notebook.py is the embedded file.
func TestRunTarCarriesLauncher(t *testing.T) {
	arch, err := runTar("r1", "a.ipynb", nil, 5, []platform.Upload{{Name: "a.ipynb", Data: []byte("{}")}})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(arch))
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "r1/.agw/agw_run_notebook.py" {
			b, _ := io.ReadAll(tr)
			found = bytes.Equal(b, launcherPy) && len(b) > 0
		}
	}
	if !found {
		t.Error("the launcher is not in the run archive or differs from the embedded file")
	}
}

func TestWorkspaceCopyName(t *testing.T) {
	if got := workspaceCopy("r20261021143005-ab12", "gpu_check.ipynb"); got != "notebooks/gpu_check_2026-10-21_14-30-05.ipynb" {
		t.Errorf("copy name: %q", got)
	}
}

func TestLinkNotebookEntry(t *testing.T) {
	n := New(Config{PublicURL: "http://hub.example"}, nil, nil, nil)
	base := "http://hub.example/user/u/srv/lab/tree/work/runs/r1"
	if got := n.Link("u", "srv", "r1", "train.ipynb"); got != base+"/executed-train.ipynb" {
		t.Errorf("notebook link: %q", got)
	}
	if got := n.Link("u", "srv", "r1", "train.py"); got != base {
		t.Errorf("script link: %q", got)
	}
}

func TestParseStatusFailedCell(t *testing.T) {
	m, _, err := parseStatus("started\t2026-01-01T10:00:00Z\nfailed_cell\t3\tZeroDivisionError\nexit_code\t1\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.FailedCell != 3 || m.CellError != "ZeroDivisionError" {
		t.Errorf("failed cell: %+v", m)
	}
	m, _, _ = parseStatus("failed_cell\tx\tboom\n")
	if m.FailedCell != 0 {
		t.Errorf("a note without a number is ignored: %+v", m)
	}
	long := strings.Repeat("e", 300)
	m, _, _ = parseStatus("failed_cell\t1\t" + long + "\x1b[31m\n")
	if len([]rune(m.CellError)) > 120 || strings.ContainsRune(m.CellError, 0x1b) {
		t.Errorf("error text not bounded or visible: %q", m.CellError)
	}
}

func TestRunMessageFailedCell(t *testing.T) {
	one, three, code := 1, 3, 1
	info := RunInfo{Entry: "a.ipynb", ExitCode: &code, DurationSeconds: 4, FailedCell: &three, CellError: "ZeroDivisionError"}
	if got := runMessage(info); got != "cell 3 failed (ZeroDivisionError) after 4 s" {
		t.Errorf("message: %q", got)
	}
	info.FailedCell, info.CellError, info.DurationSeconds = &one, "time limit", 0
	if got := runMessage(info); !strings.Contains(got, "cell 1") || !strings.Contains(got, "time limit") {
		t.Errorf("time limit message: %q", got)
	}
}

func TestApplyMarkersNotebook(t *testing.T) {
	code := 1
	info := RunInfo{RunID: "r20261021143005-ab12", Entry: "a.ipynb"}
	applyMarkers(&info, markers{ExitCode: &code, FailedCell: 2, CellError: "ValueError"})
	if info.Notebook != "executed-a.ipynb" || info.NotebookCopy != "work/notebooks/a_2026-10-21_14-30-05.ipynb" ||
		info.FailedCell == nil || *info.FailedCell != 2 || info.CellError != "ValueError" {
		t.Errorf("info: %+v", info)
	}
}

// The starter is also installed as a command in the exec image (agw-run-notebook): without a shebang the shell would
// read the docstring as shell code (found in phase B of #193).
func TestLauncherHasShebang(t *testing.T) {
	if !bytes.HasPrefix(launcherPy, []byte("#!/usr/bin/env python3\n")) {
		t.Fatalf("the starter must begin with a python3 shebang, it begins with %q", launcherPy[:min(40, len(launcherPy))])
	}
}

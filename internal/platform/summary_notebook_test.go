// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"net/http"
	"strings"
	"testing"
)

// The summary of a run says what starts, with which arguments, which datasets are unpacked first and
// which files it gets; the code itself carries the approval (NotebookPreview). Every field of the call
// is named, so nothing of it stays hidden behind a friendly sentence.
func TestSummaryNotebookRun(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/datasets/17": `{"id":17,"name":"mnist-mini"}`}}
	got := summarize(t, f, SummaryCall{Tool: "notebook_run", Method: http.MethodPost, Path: NotebookPrefix + "/runs",
		Form: map[string][]string{"entry": {"train.py"}, "args": {"--epochs", "3"}, "dataset_ids": {"17"}, "timeout_minutes": {"90"}},
		Files: []SummaryFile{
			{Field: "file", Name: "train.py", Size: 28, SHA256: "aa11"},
		}})
	wantLines(t, got,
		`Run "train.py" in your notebook server (no internet, no platform access, at most 90 min)`,
		"Arguments: --epochs 3",
		`Dataset 17 "mnist-mini" is unpacked to /home/jovyan/work/datasets/17/ before the run`,
		`File (file): "train.py" (28 bytes, sha256 aa11)`,
		"The code runs as shown in the approval above, in full.")
	if strings.Contains(got, "Also sends") {
		t.Errorf("the summary hides a field:\n%s", got)
	}
	if len(f.got) != 1 || f.got[0] != "GET /datasets/17" {
		t.Errorf("reads of the summary: %v", f.got)
	}
	// Without a reader the dataset is named by its id alone.
	bare := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: NotebookPrefix + "/runs",
		Form: map[string][]string{"entry": {"train.py"}, "dataset_ids": {"17", "18"}}})
	wantLines(t, bare, "Dataset 17 is unpacked to /home/jovyan/work/datasets/17/ before the run",
		"Dataset 18 is unpacked to /home/jovyan/work/datasets/18/ before the run",
		"at most 120 min")

	// Stopping the server is its own sentence.
	stop := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: NotebookPrefix + "/stop"})
	if !strings.HasPrefix(stop, "Stop your notebook server (files stay)") {
		t.Errorf("stop: %q", stop)
	}

	// A run whose form the runner would refuse keeps the generic template: never a friendly sentence
	// about a call the gateway cannot read.
	odd := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: NotebookPrefix + "/runs",
		Form: map[string][]string{"entry": {"train.py"}, "shell": {"rm -rf /"}}})
	if !strings.HasPrefix(odd, "Send POST "+NotebookPrefix+"/runs") || !strings.Contains(odd, "Also sends form shell") {
		t.Errorf("unreadable form: %q", odd)
	}
	// A notebook path no template knows stays with the built-in one.
	other := summarize(t, nil, SummaryCall{Method: http.MethodGet, Path: NotebookPrefix + "/runs"})
	if !strings.HasPrefix(other, "Send GET ") {
		t.Errorf("reading call: %q", other)
	}
}

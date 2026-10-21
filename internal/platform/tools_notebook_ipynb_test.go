// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

const nbBase = `{"nbformat":4,"nbformat_minor":5,"metadata":{},"cells":[
 {"cell_type":"markdown","metadata":{},"source":["# title"]},
 {"cell_type":"code","metadata":{},"execution_count":null,"outputs":[],"source":["a = 1\n","print(a)"]},
 {"cell_type":"code","metadata":{},"execution_count":null,"outputs":[],"source":"b = 2"}]}`

func TestParseRunFormNotebookArgs(t *testing.T) {
	if _, err := ParseRunForm(map[string][]string{"entry": {"t.ipynb"}, "args": {"--x"}}, 0); err == nil ||
		!strings.Contains(err.Error(), "params.json") {
		t.Errorf("a notebook with args must be refused: %v", err)
	}
	if _, err := ParseRunForm(map[string][]string{"entry": {"t.ipynb"}}, 0); err != nil {
		t.Errorf("a notebook without args: %v", err)
	}
	if _, err := ParseRunForm(map[string][]string{"entry": {"t.py"}, "args": {"--x"}}, 0); err != nil {
		t.Errorf("a script with args: %v", err)
	}
}

func TestCheckNotebookRun(t *testing.T) {
	b := RunBody{Entry: "t.ipynb"}
	ok := Upload{Name: "t.ipynb", Data: []byte(nbBase)}
	if err := CheckNotebookRun(b, []Upload{ok}); err != nil {
		t.Errorf("valid notebook: %v", err)
	}
	if err := CheckNotebookRun(b, []Upload{ok, {Name: "executed-t.ipynb", Data: []byte("{}")}}); err == nil ||
		!strings.Contains(err.Error(), "executed-t.ipynb") {
		t.Errorf("executed name must be refused: %v", err)
	}
	for name, data := range map[string]string{
		"nbformat 3":    `{"nbformat":3,"cells":[]}`,
		"source number": `{"nbformat":4,"cells":[{"cell_type":"code","source":5}]}`,
		"not json":      `x`,
	} {
		if err := CheckNotebookRun(b, []Upload{{Name: "t.ipynb", Data: []byte(data)}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := CheckNotebookRun(RunBody{Entry: "t.py"}, []Upload{{Name: "t.py", Data: []byte("x")}}); err != nil {
		t.Errorf("a script: %v", err)
	}
}

func TestNotebookCodeSHA256(t *testing.T) {
	base, ok := NotebookCodeSHA256([]byte(nbBase))
	if !ok || base == "" {
		t.Fatal("no digest")
	}
	other := strings.NewReplacer(`"# title"`, `"# other title"`, `"outputs":[]`, `"outputs":[{"output_type":"stream","name":"stdout","text":"x"}]`,
		`"metadata":{}`, `"metadata":{"tags":["skip-execution"]}`).Replace(nbBase)
	if got, _ := NotebookCodeSHA256([]byte(other)); got != base {
		t.Error("markdown, outputs and metadata must not change the digest")
	}
	if got, _ := NotebookCodeSHA256([]byte(strings.Replace(nbBase, "b = 2", "b = 3", 1))); got == base {
		t.Error("a changed code cell must change the digest")
	}
	// the same text with the cell border moved
	moved := strings.Replace(strings.Replace(nbBase, `"source":"b = 2"`, `"source":"2"`, 1), `"print(a)"`, `"print(a)b = "`, 1)
	if got, _ := NotebookCodeSHA256([]byte(moved)); got == base {
		t.Error("a moved cell border must change the digest")
	}
}

func TestNotebookPreviewNotebookHint(t *testing.T) {
	req := Request{Form: RunForm(RunBody{Entry: "t.ipynb", TimeoutMinutes: 5}),
		Uploads: []Upload{{Name: "t.ipynb", Data: []byte(nbBase), SHA256: "aa"}}}
	got, err := NotebookPreview(req, DefaultNotebookShape)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Runs t.ipynb as a notebook in your notebook server", "saved in the run directory and in notebooks/", "# cell 1", "# cell 2"} {
		if !strings.Contains(got, w) {
			t.Errorf("preview without %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "# title") {
		t.Error("markdown cells are not shown")
	}
}

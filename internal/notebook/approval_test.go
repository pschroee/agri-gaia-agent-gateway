// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"agw/internal/platform"
)

// approvalReq is the call of notebook_run as the socket hands it over: the form of the tool and the files
// it read from the sandbox.
func approvalReq(t *testing.T, body platform.RunBody, files ...platform.Upload) platform.Request {
	t.Helper()
	return platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs",
		Form: platform.RunForm(body), Uploads: files}
}

// namedDatasets answers the reading calls of a chat with the names of the datasets the test knows.
type namedDatasets struct {
	names map[int64]string
	fail  bool
	reads []string
}

func (n *namedDatasets) read(_ context.Context, r platform.Request) (platform.Result, error) {
	n.reads = append(n.reads, r.String())
	if n.fail {
		return platform.Result{Status: "error", Message: "not readable"}, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.Path, "/datasets/"), 10, 64)
	if err != nil {
		return platform.Result{Status: "error", Message: "unknown path"}, nil
	}
	name, ok := n.names[id]
	if !ok {
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{}`}, nil
	}
	doc, _ := json.Marshal(map[string]any{"id": id, "name": name})
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: string(doc)}, nil
}

// approvalDataOf builds the data of one run and reads it back.
func approvalDataOf(t *testing.T, n *Notebooks, req platform.Request, read platform.Doer) ApprovalData {
	t.Helper()
	raw, err := n.ApprovalData(context.Background(), req, read)
	if err != nil {
		t.Fatalf("ApprovalData: %v", err)
	}
	var data ApprovalData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("the data is no JSON: %s (%v)", raw, err)
	}
	return data
}

// Every file of a run appears with its size, its SHA-256 and the text the card shows; entry, datasets and
// the limits of this instance stand next to it (own code C-E4).
func TestApprovalDataFilesDatasetsLimits(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	read := &namedDatasets{names: map[int64]string{17: "mnist-mini", 18: "trainset"}}
	code := "import torch\nprint('hello')\n"
	req := approvalReq(t, platform.RunBody{Entry: "train.py", Args: []string{"--epochs", "1"}, DatasetIDs: []int64{17, 18},
		TimeoutMinutes: 30, Note: "trains the baseline for the next step"},
		platform.Upload{Field: "file", Name: "train.py", Data: []byte(code), SHA256: "aa11"},
		platform.Upload{Field: "file", Name: "weights.bin", Data: []byte{0x00, 0xff, 0xfe}, SHA256: "bb22"})

	data := approvalDataOf(t, n, req, read.read)
	if data.Entry != "train.py" {
		t.Errorf("entry: %q", data.Entry)
	}
	if l := data.Limits; l.CPUs != 2 || l.MemGB != 8 || l.TimeoutMin != 30 || l.Internet {
		t.Errorf("limits: %+v", l)
	}
	// The agent's own note travels along, unchanged: the card shows it as the agent's text.
	if data.Note != "trains the baseline for the next step" {
		t.Errorf("note: %q", data.Note)
	}
	// Without a note the field stays away.
	if plain := approvalDataOf(t, n, approvalReq(t, platform.RunBody{Entry: "train.py"},
		platform.Upload{Field: "file", Name: "train.py", Data: []byte("print(1)\n"), SHA256: "aa11"}), nil); plain.Note != "" {
		t.Errorf("a run without a note carries one: %q", plain.Note)
	}
	if len(data.Files) != 2 {
		t.Fatalf("files: %+v", data.Files)
	}
	if f := data.Files[0]; f.Name != "train.py" || f.Size != int64(len(code)) || f.SHA256 != "aa11" ||
		f.Text == nil || *f.Text != code {
		t.Errorf("first file: %+v text=%v", f, f.Text)
	}
	// A binary file gets no text: the card shows name, size and SHA-256 and says that it has no preview.
	if f := data.Files[1]; f.Name != "weights.bin" || f.Size != 3 || f.Text != nil {
		t.Errorf("second file: %+v text=%v", f, f.Text)
	}
	if len(data.Datasets) != 2 || data.Datasets[0] != (ApprovalDataset{ID: 17, Name: "mnist-mini"}) ||
		data.Datasets[1] != (ApprovalDataset{ID: 18, Name: "trainset"}) {
		t.Errorf("datasets: %+v", data.Datasets)
	}
	if len(read.reads) != 2 || read.reads[0] != "GET /datasets/17" || read.reads[1] != "GET /datasets/18" {
		t.Errorf("reads: %v", read.reads)
	}
}

// A dataset whose name the gateway cannot read keeps the id alone; the approval is built either way (the
// card never guesses a name).
func TestApprovalDataWithoutReadableDatasetName(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	req := approvalReq(t, platform.RunBody{Entry: "train.py", DatasetIDs: []int64{17, 18}},
		platform.Upload{Field: "file", Name: "train.py", Data: []byte("print(1)\n"), SHA256: "aa11"})
	for _, tc := range []struct {
		name string
		read platform.Doer
		want []ApprovalDataset
	}{
		{"no reader", nil, []ApprovalDataset{{ID: 17}, {ID: 18}}},
		{"failing read", (&namedDatasets{fail: true}).read, []ApprovalDataset{{ID: 17}, {ID: 18}}},
		{"no name in the answer", (&namedDatasets{names: map[int64]string{18: "trainset"}}).read,
			[]ApprovalDataset{{ID: 17}, {ID: 18, Name: "trainset"}}},
	} {
		data := approvalDataOf(t, n, req, tc.read)
		if len(data.Datasets) != len(tc.want) {
			t.Fatalf("%s: %+v", tc.name, data.Datasets)
		}
		for i, want := range tc.want {
			if data.Datasets[i] != want {
				t.Errorf("%s: dataset %d is %+v, expected %+v", tc.name, i, data.Datasets[i], want)
			}
		}
	}
}

// Control and bidi characters appear visibly in the card: the text of a file, the name of a file and the
// name of a dataset are escaped (Review W1).
func TestApprovalDataEscapesForDisplay(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	rlo := string(rune(0x202e)) // RIGHT-TO-LEFT OVERRIDE
	read := &namedDatasets{names: map[int64]string{17: "mnist" + rlo + "mini"}}
	// The entry is held to the rule of the tool here as well (ParseRunForm, C-E4): a name with a bidi
	// character is refused before it can reach the card. Escaped are the texts that do come through.
	if _, err := n.ApprovalData(context.Background(),
		approvalReq(t, platform.RunBody{Entry: "tra" + rlo + "in.py", DatasetIDs: []int64{17}}), read.read); err == nil {
		t.Error("an entry with a bidi character was not refused")
	}
	req := approvalReq(t, platform.RunBody{Entry: "train.py", DatasetIDs: []int64{17}},
		platform.Upload{Field: "file", Name: "tra" + rlo + "in.py",
			Data: []byte("# hidden" + rlo + " code\n"), SHA256: "aa11"})

	data := approvalDataOf(t, n, req, read.read)
	if data.Entry != "train.py" {
		t.Errorf("entry: %q", data.Entry)
	}
	if len(data.Files) != 1 || data.Files[0].Name != "tra\\u202ein.py" || data.Files[0].Text == nil ||
		*data.Files[0].Text != "# hidden\\u202e code\n" {
		t.Errorf("file: %+v", data.Files)
	}
	if len(data.Datasets) != 1 || data.Datasets[0].Name != "mnist\\u202emini" {
		t.Errorf("dataset: %+v", data.Datasets)
	}
	// The agent's note is escaped like every other text the card shows (Review W1).
	noted := approvalReq(t, platform.RunBody{Entry: "train.py", Note: "hidden" + rlo + " text"},
		platform.Upload{Field: "file", Name: "train.py", Data: []byte("print(1)\n"), SHA256: "aa11"})
	if d := approvalDataOf(t, n, noted, nil); d.Note != "hidden\\u202e text" {
		t.Errorf("note: %q", d.Note)
	}
}

// A notebook appears as its code cells in JSON: the card shows them cell by cell, the outputs of the
// notebook (they can carry megabytes of images) and the Markdown cells stay out of the approval.
func TestApprovalDataNotebookCells(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	const nb = `{"cells":[
		{"cell_type":"markdown","source":["# Title\n","text"]},
		{"cell_type":"code","source":["import torch\n","print(1)\n"],"outputs":[{"data":{"image/png":"AAAA"}}]},
		{"cell_type":"code","source":"print(2)\n","outputs":[]},
		{"cell_type":"code","source":["   \n"]}],
		"metadata":{"kernelspec":{"name":"python3"}},"nbformat":4}`
	req := approvalReq(t, platform.RunBody{Entry: "train.ipynb"},
		platform.Upload{Field: "file", Name: "train.ipynb", Data: []byte(nb), SHA256: "aa11"})

	data := approvalDataOf(t, n, req, nil)
	if len(data.Files) != 1 || data.Files[0].Text == nil {
		t.Fatalf("file without text: %+v", data.Files)
	}
	text := *data.Files[0].Text
	var out struct {
		Cells []struct {
			Type   string `json:"cell_type"`
			Source string `json:"source"`
		} `json:"cells"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("the card's text is no JSON: %s (%v)", text, err)
	}
	if len(out.Cells) != 2 {
		t.Fatalf("cells: %+v", out.Cells)
	}
	if out.Cells[0].Type != "code" || out.Cells[0].Source != "import torch\nprint(1)\n" ||
		out.Cells[1].Source != "print(2)\n" {
		t.Errorf("cells: %+v", out.Cells)
	}
	for _, w := range []string{"Title", "image/png", "AAAA", "nbformat"} {
		if strings.Contains(text, w) {
			t.Errorf("the data still carries %q: %s", w, text)
		}
	}
	// The text of the card and the preview of the call show the same code.
	preview, err := platform.NotebookPreview(req, platform.DefaultNotebookShape)
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range out.Cells {
		if !strings.Contains(preview, strings.TrimRight(cell.Source, "\n")) {
			t.Errorf("the preview does not show %q:\n%s", cell.Source, preview)
		}
	}
}

// The card and the preview agree about the files they show: a file the preview notes instead of showing
// gets no text in the data either.
func TestApprovalDataAgreesWithPreview(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	big := strings.Repeat("# a line\n", 3000) // more text than the approval carries (20 KB)
	req := approvalReq(t, platform.RunBody{Entry: "a.py", TimeoutMinutes: 5},
		platform.Upload{Field: "file", Name: "a.py", Data: []byte(big), SHA256: "aa11"},
		platform.Upload{Field: "file", Name: "b.ipynb", Data: []byte("no JSON"), SHA256: "bb22"})

	preview, err := platform.NotebookPreview(req, platform.DefaultNotebookShape)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview, "(not shown:") {
		t.Fatalf("the preview shows the text of the file that is too large:\n%s", preview)
	}
	data := approvalDataOf(t, n, req, nil)
	byName := map[string]ApprovalFile{}
	for _, f := range data.Files {
		byName[f.Name] = f
	}
	if f := byName["a.py"]; f.Text != nil {
		t.Errorf("the data carries text the preview does not show: %+v", f)
	}
	// A notebook the approval cannot read as cells stays plain text, like every other text file.
	if f := byName["b.ipynb"]; f.Text == nil || *f.Text != "no JSON" {
		t.Errorf("unreadable notebook: %+v", f)
	}
}

// A form the runner would refuse is an error, not an approval without data: the caller refuses the call.
func TestApprovalDataRefusesBadForm(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	req := platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs",
		Form: map[string][]string{"entry": {"train.py"}, "unknown": {"x"}}}
	if _, err := n.ApprovalData(context.Background(), req, nil); err == nil {
		t.Fatal("a form with an unknown field was accepted")
	}
}

// A failing read of a dataset does not fail the approval; only a form the runner refuses does.
func TestApprovalDataReadFailureIsNoError(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	req := approvalReq(t, platform.RunBody{Entry: "train.py", DatasetIDs: []int64{17}},
		platform.Upload{Field: "file", Name: "train.py", Data: []byte("print(1)\n"), SHA256: "aa11"})
	read := func(context.Context, platform.Request) (platform.Result, error) {
		return platform.Result{}, errors.New("platform down")
	}
	data := approvalDataOf(t, n, req, read)
	if len(data.Datasets) != 1 || data.Datasets[0].Name != "" {
		t.Errorf("datasets after a failing read: %+v", data.Datasets)
	}
}

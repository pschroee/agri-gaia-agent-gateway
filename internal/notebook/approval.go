// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"agw/internal/platform"
)

// The structured data of the approval of a run (own code C-E4): with it the card in the frontend shows
// the code of a run file by file instead of the text of the preview. Everything here comes from the call
// the user is asked about — the form of the run, the files the socket read (their SHA-256 stands in the
// approval) and the names the gateway reads from the platform — never from the agent's text. The shape
// is the one the plan of C-E4 fixes: entry, files, datasets, limits.

// ApprovalData is the value of the field "notebook" of an approval. The strings inside are escaped for
// display (platform.VisibleControls): the card sets them as code, and a control or bidi character in them
// would reorder or hide what the user approves (Review W1).
type ApprovalData struct {
	Entry    string            `json:"entry"`
	Files    []ApprovalFile    `json:"files"`
	Datasets []ApprovalDataset `json:"datasets"`
	Limits   ApprovalLimits    `json:"limits"`
	// Note is the agent's own note to the user about what this run is for, exactly as the agent wrote it
	// (only escaped for display) and **not** something the gateway read or checked. The card shows it in a
	// block of its own, marked "not verified" — the point of the field is that the two voices stay apart.
	Note string `json:"note,omitempty"`
}

// ApprovalFile is one file of the run as it was approved: name, size and SHA-256 always, the text while
// the card may show it (see approvalText). Text is missing for a file the approval does not carry as
// text (a binary, or more text than the preview shows).
type ApprovalFile struct {
	Name   string  `json:"name"`
	Size   int64   `json:"size"`
	SHA256 string  `json:"sha256"`
	Text   *string `json:"text,omitempty"`
}

// ApprovalDataset is a dataset the run gets. Name is what the platform reports now; it stays empty when
// the gateway could not read it — the card shows the id alone, never a guessed name.
type ApprovalDataset struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// ApprovalLimits is what a run of this instance gets: the numbers the spawner gives the agent's servers
// (the capability text names the same ones) and the time limit the call asked for.
type ApprovalLimits struct {
	CPUs       int  `json:"cpus"`
	MemGB      int  `json:"mem_gb"`
	TimeoutMin int  `json:"timeout_min"`
	Internet   bool `json:"internet"`
}

// The shape of a notebook server of this instance.
const (
	serverCPUs  = 2
	serverMemGB = 8
)

// ApprovalData builds the structured data of the approval of a run. read answers the reading calls of the
// chat's user (nil: the names of the datasets stay empty); the caller passes the reader it wrote the
// summary with, so these reads appear in the summary's activity entry and not as calls of their own. A
// form the runner would refuse is an error: the caller turns it into a refusal, never into an approval
// without data.
func (n *Notebooks) ApprovalData(ctx context.Context, req platform.Request, read platform.Doer) (json.RawMessage, error) {
	body, err := platform.ParseRunForm(req.Form, n.timeoutMax())
	if err != nil {
		return nil, err
	}
	data := ApprovalData{
		Entry: platform.VisibleControls(body.Entry),
		Note:  platform.VisibleControls(body.Note),
		// The server of this instance has no internet and no access to the platform; the place is here
		// rather than in the card, so that the card shows what the gateway states.
		Limits:   ApprovalLimits{CPUs: serverCPUs, MemGB: serverMemGB, TimeoutMin: body.TimeoutMinutes, Internet: false},
		Files:    make([]ApprovalFile, 0, len(req.Uploads)),
		Datasets: make([]ApprovalDataset, 0, len(body.DatasetIDs)),
	}
	for _, u := range req.Uploads {
		data.Files = append(data.Files, approvalFile(u))
	}
	for _, id := range body.DatasetIDs {
		name, _ := n.datasetName(ctx, read, id)
		data.Datasets = append(data.Datasets, ApprovalDataset{ID: id, Name: platform.VisibleControls(name)})
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// approvalFile turns one approved file into its entry of the card.
func approvalFile(u platform.Upload) ApprovalFile {
	f := ApprovalFile{Name: platform.VisibleControls(u.Name), Size: int64(len(u.Data)), SHA256: u.SHA256}
	if text, ok := approvalText(u.Name, u.Data); ok {
		f.Text = &text
	}
	return f
}

// approvalText is the text the card may show of one file, and whether it shows one. The rule is the one
// of the preview (platform.NotebookPreview): a notebook appears as its code cells, another file as its
// text while the approval shows it at all. A file the preview shows without text (not valid UTF-8, or
// more text than it carries) gets no text here either — card and preview never disagree about what the
// user sees of a file.
func approvalText(name string, data []byte) (string, bool) {
	if strings.HasSuffix(strings.ToLower(name), ".ipynb") {
		if cells, ok := notebookCellsJSON(data); ok {
			return cells, true
		}
	}
	if !utf8.Valid(data) || len(data) > platform.NotebookTextMax {
		return "", false
	}
	return platform.VisibleControls(string(data)), true
}

// notebookCellsJSON rebuilds a notebook for the card: its code cells with their source, and nothing else
// (the outputs of a notebook can carry megabytes of images that no card shows). The sources are escaped
// as JSON (platform.VisibleControlsJSON), so a reader that parses the text never gets a hidden character
// back. ok false: not a notebook the preview shows as cells, then the caller uses the plain text.
func notebookCellsJSON(data []byte) (string, bool) {
	var nb struct {
		Cells []struct {
			Type   string          `json:"cell_type"`
			Source json.RawMessage `json:"source"`
		} `json:"cells"`
	}
	if json.Unmarshal(data, &nb) != nil || len(nb.Cells) == 0 {
		return "", false
	}
	type cell struct {
		Type   string `json:"cell_type"`
		Source string `json:"source"`
	}
	out := struct {
		Cells []cell `json:"cells"`
	}{Cells: []cell{}}
	for _, c := range nb.Cells {
		if c.Type != "code" {
			continue
		}
		src, ok := platform.NotebookSource(c.Source)
		if !ok || strings.TrimSpace(src) == "" {
			continue
		}
		out.Cells = append(out.Cells, cell{Type: "code", Source: platform.VisibleControlsJSON(src)})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// datasetName reads the name of a dataset as the platform reports it now. ok false: no name readable (no
// platform binding, a failing call, an answer without a name) — the card then shows the id alone.
func (n *Notebooks) datasetName(ctx context.Context, read platform.Doer, id int64) (string, bool) {
	if read == nil {
		return "", false
	}
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10)})
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return "", false
	}
	var doc struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(res.Body), &doc) != nil || doc.Name == "" {
		return "", false
	}
	return doc.Name, true
}

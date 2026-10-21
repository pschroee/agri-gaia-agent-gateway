// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The notebook tools run the agent's own code in the chat's notebook server (plan "own code" C-E3).
// They work on virtual paths under NotebookPrefix, which the gateway answers itself (G1.5): the
// delegation, the effect class, the approval, the plan and the log apply without a special way.
// Their descriptions come from the plan's table, word for word.

// NotebookPrefix is the virtual path prefix of the notebook tools (own code C-E3). The manager
// answers it through its own doer (RegisterVirtual) or, without one, with "not configured".
const NotebookPrefix = "/_agw/notebook"

// Limits of one run (C-E3). They are the same for the tool (before the file is read) and for the
// runner (before the container command), so that the agent's refusal and the runner's agree.
const (
	NotebookMaxFiles     = 50
	NotebookFileMaxBytes = 1 << 20 // one source file
	NotebookFilesMax     = 4 << 20 // all files of a run together
	NotebookMaxArgs      = 20
	NotebookMaxDatasets  = 3
	// NotebookTimeoutMinutes: what a run gets when it asks for nothing, and the ceiling until the
	// installed setup configures a lower one (AGW_NOTEBOOK_TIMEOUT_MAX_MIN).
	NotebookTimeoutMinutes = 120
	NotebookTimeoutMax     = 480
	// NotebookNoteMax is how long the agent's own note to a run may be (own code C-E4). The note is the
	// agent's text: the user sees it as such, marked as not verified, and the gateway never states it as
	// something it checked.
	NotebookNoteMax = 500
)

// notebookExtensions are the endings a run accepts: code and small configuration, no data (that
// comes as a dataset).
var notebookExtensions = []string{".py", ".ipynb", ".txt", ".json", ".yaml", ".yml", ".csv", ".md", ".cfg", ".toml"}

var (
	notebookEntryRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	notebookArgRe   = regexp.MustCompile(`^[A-Za-z0-9_.,=:/+-]{1,200}$`)
	NotebookRunIDRe = regexp.MustCompile(`^r[0-9]{14}-[0-9a-f]{4}$`)
	// notebookPathRe is the path of a file inside a run directory (notebook_fetch): plain segments,
	// no dot segment, at most five of them.
	notebookPathRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+){0,4}$`)
)

// RunBody is what a notebook run needs besides its files. The agent sends it as form fields; the
// tool, the delegation and the runner read it with ParseRunForm, so all three see the same values.
type RunBody struct {
	Entry          string
	Args           []string
	DatasetIDs     []int64
	TimeoutMinutes int
	// Note is the agent's own short note to the user about what the run is for (own code C-E4). It is
	// shown as the agent's text and marked as not verified; it is not part of what the run does and the
	// gateway never mixes it into what it states itself.
	Note string
}

func init() {
	Tools = append(Tools, notebookStatus, notebookRun, notebookRuns, notebookRunStatus, notebookLog, notebookFetch, notebookStop)
}

// ParseRunForm reads the form fields of a run and checks them: entry, args, dataset_ids and
// timeout_minutes, and nothing else. maxTimeoutMin is the ceiling of the installed setup
// (AGW_NOTEBOOK_TIMEOUT_MAX_MIN, 0: NotebookTimeoutMax). The files are checked separately
// (ValidateRunFiles), because the tool knows only their paths.
func ParseRunForm(form map[string][]string, maxTimeoutMin int) (RunBody, error) {
	b := RunBody{TimeoutMinutes: NotebookTimeoutMinutes}
	if maxTimeoutMin <= 0 {
		maxTimeoutMin = NotebookTimeoutMax
	}
	for k, vs := range form {
		switch k {
		case "entry", "args", "dataset_ids", "timeout_minutes", "note":
		default:
			return b, fmt.Errorf("unknown field %q (entry, args, dataset_ids, timeout_minutes, note)", k)
		}
		if k != "args" && k != "dataset_ids" && len(vs) != 1 {
			return b, fmt.Errorf("field %q given %d times", k, len(vs))
		}
	}
	entry := form["entry"]
	if len(entry) == 0 || strings.TrimSpace(entry[0]) == "" {
		return b, errors.New("entry missing: the file name of the script or notebook to run")
	}
	b.Entry = strings.TrimSpace(entry[0])
	if !notebookEntryRe.MatchString(b.Entry) {
		return b, fmt.Errorf("entry %q is not a plain file name (letters, digits, _ . -; at most 64 characters)", b.Entry)
	}
	for _, a := range form["args"] {
		if !notebookArgRe.MatchString(a) {
			return b, fmt.Errorf("argument %q is not allowed (letters, digits and _.,=:/+- only, at most 200 characters)", a)
		}
		b.Args = append(b.Args, a)
	}
	if len(b.Args) > NotebookMaxArgs {
		return b, fmt.Errorf("at most %d arguments", NotebookMaxArgs)
	}
	if len(b.Args) > 0 && strings.EqualFold(pathExt(b.Entry), ".ipynb") {
		// A notebook is run as the user approved its cells; an argument could only be put in by changing a cell
		// (papermill does) or be ignored. The settings of a notebook come from a file such as params.json.
		return b, errors.New("args are only for a .py entry; a notebook reads its settings from a file such as params.json")
	}
	for _, v := range form["dataset_ids"] {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id < 0 {
			return b, fmt.Errorf("dataset_ids: %q is not a dataset id", v)
		}
		if !slices.Contains(b.DatasetIDs, id) {
			b.DatasetIDs = append(b.DatasetIDs, id)
		}
	}
	if len(b.DatasetIDs) > NotebookMaxDatasets {
		return b, fmt.Errorf("at most %d datasets per run", NotebookMaxDatasets)
	}
	if t := form["timeout_minutes"]; len(t) > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(t[0]))
		if err != nil || n < 1 {
			return b, fmt.Errorf("timeout_minutes: %q is not a number of minutes", t[0])
		}
		if n > maxTimeoutMin {
			return b, fmt.Errorf("timeout_minutes: at most %d minutes in this setup", maxTimeoutMin)
		}
		b.TimeoutMinutes = n
	}
	if note := form["note"]; len(note) > 0 {
		// The agent's own words: no regexp, only what is safe to show and store (valid UTF-8, and short
		// enough for the card). Whitespace around it and a note that is only whitespace stay nothing.
		if !utf8.ValidString(note[0]) {
			return b, errors.New("note: not valid UTF-8")
		}
		noteText := strings.TrimSpace(note[0])
		if n := utf8.RuneCountInString(noteText); n > NotebookNoteMax {
			return b, fmt.Errorf("note: at most %d characters, %d given", NotebookNoteMax, n)
		}
		b.Note = noteText
	}
	return b, nil
}

// RunForm turns a run into the form fields the notebook paths carry. Both the tool and the runner
// build the form this way, so the request of the tool and what the runner reads are the same shape.
func RunForm(b RunBody) map[string][]string {
	form := map[string][]string{}
	if b.Entry != "" {
		form["entry"] = []string{b.Entry}
	}
	if len(b.Args) > 0 {
		form["args"] = b.Args
	}
	if len(b.DatasetIDs) > 0 {
		ids := make([]string, 0, len(b.DatasetIDs))
		for _, id := range b.DatasetIDs {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		form["dataset_ids"] = ids
	}
	if b.TimeoutMinutes > 0 {
		form["timeout_minutes"] = []string{strconv.Itoa(b.TimeoutMinutes)}
	}
	if b.Note != "" {
		form["note"] = []string{b.Note}
	}
	return form
}

// ValidateRunFiles checks the files of a run: at most NotebookMaxFiles, the allowed endings, no two
// files of the same base name (they are copied flat), a size per file and in total, and the entry
// must be one of them. sizes may be nil (the tool checks the paths before anything is read).
func ValidateRunFiles(b RunBody, names []string, sizes []int64) error {
	if len(names) == 0 {
		return errors.New("files: at least one file")
	}
	if len(names) > NotebookMaxFiles {
		return fmt.Errorf("at most %d files per run, %d given", NotebookMaxFiles, len(names))
	}
	seen := map[string]bool{}
	var total int64
	for i, name := range names {
		if strings.Contains(name, "/") || name == "" || name == "." || name == ".." {
			return fmt.Errorf("file %q: give it as a plain file name", name)
		}
		if !slices.Contains(notebookExtensions, strings.ToLower(pathExt(name))) {
			return fmt.Errorf("file %q: allowed are %s (data belongs in a dataset, not in a run)", name, strings.Join(notebookExtensions, " "))
		}
		if seen[name] {
			return fmt.Errorf("two files named %q: they are copied flat into the run directory", name)
		}
		seen[name] = true
		if sizes != nil {
			if sizes[i] > NotebookFileMaxBytes {
				return fmt.Errorf("file %q is larger than %d MB", name, NotebookFileMaxBytes>>20)
			}
			total += sizes[i]
		}
	}
	if total > NotebookFilesMax {
		return fmt.Errorf("the files together are larger than %d MB", NotebookFilesMax>>20)
	}
	if !seen[b.Entry] {
		return fmt.Errorf("entry %q is not one of the files", b.Entry)
	}
	return nil
}

// CheckNotebookRun checks the files of a run that the names alone cannot show: no file may carry the name of
// the executed notebook (the run would overwrite it), and a .ipynb entry must be a notebook the launcher accepts.
func CheckNotebookRun(b RunBody, files []Upload) error {
	for _, f := range files {
		if strings.HasPrefix(f.Name, "executed-") && strings.EqualFold(pathExt(f.Name), ".ipynb") && f.Name == "executed-"+b.Entry {
			return fmt.Errorf("%s is the name of the executed notebook; rename the file", f.Name)
		}
	}
	if strings.EqualFold(pathExt(b.Entry), ".ipynb") {
		for _, f := range files {
			if f.Name == b.Entry {
				return CheckNotebook(f.Data)
			}
		}
	}
	return nil
}

// notebookDoc is what the code of a notebook is read from.
type notebookDoc struct {
	NBFormat int `json:"nbformat"`
	Cells    []struct {
		Type   string          `json:"cell_type"`
		Source json.RawMessage `json:"source"`
	} `json:"cells"`
}

// CheckNotebook says whether data is a notebook in nbformat 4 whose code cells have source text. The launcher
// checks the same again (and refuses duplicate keys) before it runs anything.
func CheckNotebook(data []byte) error {
	var nb notebookDoc
	if json.Unmarshal(data, &nb) != nil || nb.NBFormat != 4 || nb.Cells == nil {
		return errors.New("the entry is not a notebook in nbformat 4 (cells with source text)")
	}
	for _, c := range nb.Cells {
		if c.Type != "code" {
			continue
		}
		if _, ok := NotebookSource(c.Source); !ok {
			return errors.New("the entry is not a notebook in nbformat 4 (cells with source text)")
		}
	}
	return nil
}

// codeCells lists the source of the code cells that are run: those with text, in order.
func codeCells(data []byte) ([]string, bool) {
	var nb notebookDoc
	if json.Unmarshal(data, &nb) != nil || len(nb.Cells) == 0 {
		return nil, false
	}
	var out []string
	for _, c := range nb.Cells {
		if c.Type != "code" {
			continue
		}
		src, ok := NotebookSource(c.Source)
		if !ok || strings.TrimSpace(src) == "" {
			continue
		}
		out = append(out, src)
	}
	return out, true
}

// NotebookCodeSHA256 binds a notebook by its code cells only: markdown, metadata and outputs are not part of it,
// because the launcher runs nothing but the source of the code cells. Each cell enters with its length, so that
// cell borders cannot move without changing the digest.
func NotebookCodeSHA256(data []byte) (string, bool) {
	cells, ok := codeCells(data)
	if !ok {
		return "", false
	}
	h := sha256.New()
	for _, src := range cells {
		fmt.Fprintf(h, "%d\n%s", len(src), src)
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// pathExt is filepath.Ext without the import (the names carry no directory).
func pathExt(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[i:]
	}
	return ""
}

// NotebookCallRefusal refuses raw calls to the notebook paths (own code C-E3): only the notebook
// tools may go there, because a run gets its files as sandbox paths the tool has to read before the
// user approves them. The REST binding and the raw tool request have no way to do that.
func NotebookCallRefusal(r Request) string {
	p := strings.ToLower(r.Path)
	if p == NotebookPrefix || strings.HasPrefix(p, NotebookPrefix+"/") {
		return "notebook calls go through the notebook tools ({{tool:notebook_status}}, {{tool:notebook_run}}, {{tool:notebook_runs}}, " +
			"{{tool:notebook_run_status}}, {{tool:notebook_log}}, {{tool:notebook_fetch}}, {{tool:notebook_stop}}); files of a run are read from the sandbox"
	}
	return ""
}

// --- the tools ---

var notebookStatus = Tool{
	Name: "notebook_status", CLI: "notebook-status",
	Desc: "State of this chat's notebook server in JupyterHub (not started, starting, running), its limits and the link the user can open. " +
		"The server runs your code without internet and without platform access.",
	build: func(Args) (Request, error) { return Request{Method: http.MethodGet, Path: NotebookPrefix}, nil },
}

var notebookRun = Tool{
	Name: "notebook_run", CLI: "notebook-run",
	Desc: "Runs your own code in this chat's notebook server in JupyterHub (2 CPUs, 8 GB, a shared GPU if this instance has one - {{tool:notebook_status}} says, no internet, no platform access). " +
		"Effect compute: the user approves every run and sees all code; a plan never covers it. Platform datasets in dataset_ids are unpacked to " +
		"/home/jovyan/work/datasets/<id>/ before the run. Test the code in the sandbox on a small sample first. " +
		"One run at a time; returns the run id and a link for the user. A note of yours (optional) is shown to the user " +
		"as your own text, marked as not verified.",
	Params: []Param{
		{Name: "files", Type: "array", Required: true, Path: true, Desc: "files from the sandbox, copied flat into a new run directory: code and small config files " +
			"(.py .ipynb .txt .json .yaml .yml .csv .md .cfg .toml), at most 50 files and 4 MB"},
		{Name: "entry", Type: "string", Required: true, Desc: "file name of the script or notebook to run, one of files"},
		{Name: "args", Type: "array", Desc: "command line arguments for a .py entry (a notebook takes none), letters, digits and _.,=:/+- only"},
		{Name: "dataset_ids", Type: "array", Desc: "ids of platform datasets to unpack before the run, at most 3"},
		{Name: "timeout_minutes", Type: "integer", Desc: "at most this many minutes for the run (default 120)"},
		{Name: "note", Type: "string", Desc: "your own short note to the user about what this run is for (at most 500 characters). " +
			"They see it as your note and it is marked as not verified; keep facts you claim out of it"},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		paths, err := a.strings("files")
		if err != nil {
			return Request{}, err
		}
		if len(paths) == 0 {
			return Request{}, errors.New("files: at least one file")
		}
		entry, err := requiredStr(a, "entry")
		if err != nil {
			return Request{}, err
		}
		args, err := a.strings("args")
		if err != nil {
			return Request{}, err
		}
		ids, err := a.strings("dataset_ids")
		if err != nil {
			return Request{}, err
		}
		minutes, hasMinutes, err := a.int("timeout_minutes")
		if err != nil {
			return Request{}, err
		}
		note, _, err := a.str("note")
		if err != nil {
			return Request{}, err
		}
		body := RunBody{Entry: entry, Args: args, Note: note}
		for _, v := range ids {
			id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil || id < 0 {
				return Request{}, fmt.Errorf("dataset_ids: %q is not a dataset id", v)
			}
			body.DatasetIDs = append(body.DatasetIDs, id)
		}
		body.TimeoutMinutes = NotebookTimeoutMinutes
		if hasMinutes {
			if minutes < 1 {
				return Request{}, errors.New("timeout_minutes must be positive")
			}
			body.TimeoutMinutes = int(minutes)
		}
		parsed, err := ParseRunForm(RunForm(body), NotebookTimeoutMax)
		if err != nil {
			return Request{}, err
		}
		names := make([]string, 0, len(paths))
		files := make([]File, 0, len(paths))
		for _, p := range paths {
			names = append(names, baseName(p))
			files = append(files, File{Field: "file", Path: p})
		}
		if err := ValidateRunFiles(parsed, names, nil); err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: NotebookPrefix + "/runs", Form: RunForm(parsed), Files: files}, nil
	},
}

var notebookRuns = Tool{
	Name: "notebook_runs", CLI: "notebook-runs",
	Desc: "Runs of this chat's notebook server with state and exit code.",
	build: func(Args) (Request, error) {
		return Request{Method: http.MethodGet, Path: NotebookPrefix + "/runs"}, nil
	},
}

var notebookRunStatus = Tool{
	Name: "notebook_run_status", CLI: "notebook-run-status",
	Desc: "State, exit code, duration and the files of a run.",
	Params: []Param{
		{Name: "run_id", Type: "string", Required: true, Desc: "id of the run (r<date>-<hex>, from {{tool:notebook_run}})"},
	},
	build: func(a Args) (Request, error) {
		id, err := notebookRunID(a)
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodGet, Path: NotebookPrefix + "/runs/" + id}, nil
	},
}

var notebookLog = Tool{
	Name: "notebook_log", CLI: "notebook-log",
	Desc: "End of the output (stdout and stderr) of a run, by default the last 200 lines.",
	Params: []Param{
		{Name: "run_id", Type: "string", Required: true, Desc: "id of the run (r<date>-<hex>, from {{tool:notebook_run}})"},
		{Name: "tail", Type: "integer", Desc: "the last N lines (default 200, at most 2000)"},
	},
	build: func(a Args) (Request, error) {
		id, err := notebookRunID(a)
		if err != nil {
			return Request{}, err
		}
		r := Request{Method: http.MethodGet, Path: NotebookPrefix + "/runs/" + id + "/log"}
		n, ok, err := a.int("tail")
		if err != nil {
			return Request{}, err
		}
		if ok {
			if n < 1 || n > NotebookLogMaxTail {
				return Request{}, fmt.Errorf("tail: 1 to %d lines", NotebookLogMaxTail)
			}
			r.Query = map[string]string{"tail": strconv.FormatInt(n, 10)}
		}
		return r, nil
	},
}

var notebookFetch = Tool{
	Name: "notebook_fetch", CLI: "notebook-fetch",
	Desc: "Copies a result file of a run (at most 64 MB) into the sandbox under /workspace/notebook/<run_id>/; " +
		"upload a model from there with {{tool:upload_model}}.",
	Params: []Param{
		{Name: "run_id", Type: "string", Required: true, Desc: "id of the run (r<date>-<hex>, from {{tool:notebook_run}})"},
		{Name: "path", Type: "string", Required: true, Desc: "path of the file inside the run directory, e.g. model.onnx or out/model.onnx"},
	},
	build: func(a Args) (Request, error) {
		id, err := notebookRunID(a)
		if err != nil {
			return Request{}, err
		}
		p, err := requiredStr(a, "path")
		if err != nil {
			return Request{}, err
		}
		if err := CheckNotebookFilePath(p); err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodGet, Path: NotebookPrefix + "/runs/" + id + "/file", Query: map[string]string{"path": p}}, nil
	},
}

var notebookStop = Tool{
	Name: "notebook_stop", CLI: "notebook-stop",
	Desc:  "Stops this chat's notebook server (files stay). Effect write.",
	Write: true,
	build: func(Args) (Request, error) {
		return Request{Method: http.MethodPost, Path: NotebookPrefix + "/stop"}, nil
	},
}

// CheckNotebookFilePath checks the path of a file inside a run directory: plain segments, no dot
// segment, at most five of them (notebook_fetch; the runner checks it again with realpath).
func CheckNotebookFilePath(p string) error {
	if !notebookPathRe.MatchString(p) {
		return fmt.Errorf("path %q invalid: at most five plain segments (letters, digits, _ . -), no . and no ..", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path %q invalid: . and .. are not allowed", p)
		}
	}
	return nil
}

// notebookRunID reads the run_id argument and checks its shape (NotebookRunIDRe); the delegation's
// route uses the same pattern.
func notebookRunID(a Args) (string, error) {
	id, err := requiredStr(a, "run_id")
	if err != nil {
		return "", err
	}
	if !NotebookRunIDRe.MatchString(id) {
		return "", fmt.Errorf("run_id %q invalid: {{tool:notebook_runs}} lists the runs of this chat", id)
	}
	return id, nil
}

// baseName is path.Base without the import (the sockets give the paths).
func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// --- the approval ---

const (
	// NotebookLogMaxTail is the longest log the tool asks for.
	NotebookLogMaxTail = 2000
	// NotebookTextMax is how much of a file the approval shows when it is not code: enough for a
	// configuration, not enough to drown the code above it.
	NotebookTextMax = 20 << 10
)

// NotebookShape is what a notebook server of this instance gets. It is stated in one place; the
// approval, the capability, notebook_status and the tool text all take their words from it.
type NotebookShape struct {
	CPUs, MemGB int
	// GPU: the spawner gives the agent's servers a GPU (AGW_NOTEBOOK_GPUS); it is shared with the
	// platform's trainings.
	GPU bool
}

// DefaultNotebookShape is the shape without a configuration.
var DefaultNotebookShape = NotebookShape{CPUs: 2, MemGB: 8}

// Line names the shape in one sentence fragment.
func (s NotebookShape) Line() string {
	gpu := "no GPU"
	if s.GPU {
		gpu = "a GPU (shared)"
	}
	return fmt.Sprintf("%d CPUs, %d GB, %s, no internet, no platform access", s.CPUs, s.MemGB, gpu)
}

// NotebookPreview is the body of the approval of a run (own code C-E3): the user must see the code
// that will run, in full, with the size and SHA-256 of every file. Unlike other calls the approval
// of a run is not truncated; a run whose code does not fit is refused instead (maxNotebookPreview
// in the chat package).
func NotebookPreview(r Request, s NotebookShape) (string, error) {
	b, err := ParseRunForm(r.Form, NotebookTimeoutMax)
	if err != nil {
		return "", err
	}
	if err := CheckNotebookRun(b, r.Uploads); err != nil {
		return "", err
	}
	var sb strings.Builder
	if strings.EqualFold(pathExt(b.Entry), ".ipynb") {
		fmt.Fprintf(&sb, "Runs %s as a notebook in your notebook server: %s, at most %d minutes.\n"+
			"The executed notebook with all outputs is saved in the run directory and in notebooks/.\n",
			b.Entry, s.Line(), b.TimeoutMinutes)
	} else {
		fmt.Fprintf(&sb, "Runs %s in your notebook server: %s, at most %d minutes.\n",
			b.Entry, s.Line(), b.TimeoutMinutes)
	}
	if len(b.Args) > 0 {
		fmt.Fprintf(&sb, "Arguments: %s\n", strings.Join(b.Args, " "))
	}
	if len(b.DatasetIDs) > 0 {
		ids := make([]string, 0, len(b.DatasetIDs))
		for _, id := range b.DatasetIDs {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		fmt.Fprintf(&sb, "Datasets unpacked before the run: %s (each to /home/jovyan/work/datasets/<id>/)\n", strings.Join(ids, ", "))
	}
	for _, u := range r.Uploads {
		fmt.Fprintf(&sb, "\n── %s (%d bytes, sha256 %s) ──\n", u.Name, len(u.Data), u.SHA256)
		sb.WriteString(notebookFileText(u.Name, u.Data))
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// notebookFileText shows a file of a run in the approval: the whole text for a script, the code
// cells of a notebook, the text of other files while it is small, and a note when nothing is shown.
func notebookFileText(name string, data []byte) string {
	ext := strings.ToLower(pathExt(name))
	if ext == ".ipynb" {
		if cells, ok := NotebookCodeText(data); ok {
			return cells
		}
	}
	if !utf8.Valid(data) {
		return "(not shown: not valid UTF-8)\n"
	}
	if len(data) > NotebookTextMax {
		return fmt.Sprintf("(not shown: %d bytes of text, more than %d KB)\n", len(data), NotebookTextMax>>10)
	}
	return string(data) + "\n"
}

// NotebookCodeText lists the source of the code cells of a notebook, as the user will run them
// ("# cell n", cells without text are not counted).
func NotebookCodeText(data []byte) (string, bool) {
	cells, ok := codeCells(data)
	if !ok {
		return "", false
	}
	var sb strings.Builder
	for i, src := range cells {
		fmt.Fprintf(&sb, "# cell %d\n", i+1)
		sb.WriteString(src)
		if !strings.HasSuffix(src, "\n") {
			sb.WriteString("\n")
		}
	}
	if len(cells) == 0 {
		return "(the notebook has no code cells)\n", true
	}
	return sb.String(), true
}

// NotebookSource reads the source of a cell: a list of lines or one string.
func NotebookSource(raw json.RawMessage) (string, bool) {
	var lines []string
	if json.Unmarshal(raw, &lines) == nil {
		return strings.Join(lines, ""), true
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one, true
	}
	return "", false
}

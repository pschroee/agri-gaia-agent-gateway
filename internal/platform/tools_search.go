// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// The tools of the training search (issue #168): a search with a budget that the user approves once and inside
// which the agent runs trial after trial. propose_search, search_next, search_status and end_search work on
// virtual paths under SearchPrefix, which the manager answers with the search service (G1.5, RegisterVirtual;
// the proposal itself is taken by Manager.ProposeSearch); without the service the paths answer "not available".
// clone_training is an ordinary platform call (POST /train/containers/{id}/clone of the backend fork).
// init appends the tools, so that other features can add theirs in parallel.

// SearchPrefix is the virtual path prefix of the training search (issue #168). A POST to the prefix itself is
// the proposal; the paths below it belong to the running search.
const SearchPrefix = "/_agw/search"

// SearchIDRe is the shape of the id of a search ("s" and twelve hex digits).
var SearchIDRe = regexp.MustCompile(`^s[0-9a-f]{12}$`)

// trainCloneRe is the path of the copy of a training container (clone_training); its train_config is checked like
// that of create_training (blockedCall), because the entrypoint of the container puts it into a shell command.
var trainCloneRe = regexp.MustCompile(`^/train/containers/[^/]+/clone$`)

// SearchNoteMax is how long the agent's own note to a trial or a search may be; the note is the agent's text and
// is shown as not verified.
const SearchNoteMax = 500

func init() {
	Tools = append(Tools, proposeSearch, searchNext, searchStatus, endSearch, cloneTraining)
}

// SearchCallRefusal refuses raw calls to the search paths: the proposal reads the files of the sandbox, and the
// gateway alone decides what a trial may run, so only the search tools may go there.
func SearchCallRefusal(r Request) string {
	p := strings.ToLower(r.Path)
	if p == SearchPrefix || strings.HasPrefix(p, SearchPrefix+"/") || p == CurvesPath {
		return "training search calls go through the search tools ({{tool:propose_search}}, {{tool:search_next}}, {{tool:search_status}}, {{tool:end_search}}, {{tool:training_curves}})"
	}
	return ""
}

func searchID(a Args) (string, error) {
	id, err := requiredStr(a, "search_id")
	if err != nil {
		return "", err
	}
	if !SearchIDRe.MatchString(id) {
		return "", errors.New("search_id is not a search id")
	}
	return id, nil
}

func searchNote(a Args) (string, error) {
	note, _, err := a.str("note")
	if err != nil {
		return "", err
	}
	if n := len([]rune(note)); n > SearchNoteMax {
		return "", fmt.Errorf("note: at most %d characters, %d given", SearchNoteMax, n)
	}
	return note, nil
}

var proposeSearch = Tool{
	Name: "propose_search", CLI: "propose-search",
	Desc: `Proposes a training search for ONE approval by the user: the user approves a search space and a budget, and you then run trial after trial on your own. ` +
		`space maps each hyperparameter that may change to {"type":"float"|"int","min","max","scale":"log"|"linear"} or {"type":"choice","values":[…]} or {"type":"bool"}; ` +
		`everything else stays as in the base. budget = {"max_runs","max_compute_minutes","target","patience"}, each optional, the first one reached ends the search; ` +
		`every trial uses one compute call. path "template": base_container_id is a built training container of this chat, each trial is a copy of it with other train_config values. ` +
		`path "notebook": the files are fixed for the whole search (SHA-256); a trial may only change params.json, any code change asks the user again. ` +
		`The winner is chosen by the validation metric only; when the search ends you run the final test once for the winner ({{tool:assess_model}} final=true). Call {{tool:capabilities}} first.`,
	Params: []Param{
		{Name: "title", Type: "string", Required: true, Desc: "short title of the search (at most 120 characters)"},
		{Name: "path", Type: "string", Required: true, Desc: `"template" (copies of a built training container) or "notebook" (your code in the notebook server)`},
		{Name: "space", Type: "object", Required: true, Desc: `the parameters that may change, each as {"type":"float"|"int","min","max","scale"} or {"type":"choice","values":[…]} or {"type":"bool"}; at most 8`},
		{Name: "budget", Type: "object", Desc: `{"max_runs","max_compute_minutes","target","patience"}, each optional`},
		{Name: "objective", Type: "object", Desc: `{"metric":"val_acc"|"val_loss"|"val_f1"} (default val_acc); a validation figure, nothing the model was trained on`},
		{Name: "base_container_id", Type: "integer", Desc: `path "template": id of a built training container of this chat`},
		{Name: "files", Type: "array", Path: true, Desc: `path "notebook": files from the sandbox that stay fixed for the whole search (code and small config files; params.json is not one of them)`},
		{Name: "entry", Type: "string", Desc: `path "notebook": file name of the script to run, one of files; it reads params.json and writes outputs/metrics.jsonl`},
		{Name: "args", Type: "array", Desc: `path "notebook": command line arguments for a .py entry`},
		{Name: "dataset_ids", Type: "array", Desc: `path "notebook": ids of platform datasets to unpack before each run, at most 3; never the source of a {{tool:split_dataset}}`},
		{Name: "timeout_minutes", Type: "integer", Desc: `path "notebook": at most this many minutes per run (default 120)`},
		{Name: "note", Type: "string", Desc: "your own short note to the user about the search (at most 500 characters); shown as your note, marked as not verified"},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		title, err := requiredStr(a, "title")
		if err != nil {
			return Request{}, err
		}
		path, err := requiredStr(a, "path")
		if err != nil {
			return Request{}, err
		}
		form := map[string][]string{"title": {title}, "path": {path}}
		for _, k := range []string{"space", "budget", "objective"} {
			raw, ok, err := a.object(k)
			if err != nil {
				return Request{}, err
			}
			if !ok {
				if k == "space" {
					return Request{}, errors.New("space missing")
				}
				continue
			}
			form[k] = []string{string(raw)}
		}
		note, err := searchNote(a)
		if err != nil {
			return Request{}, err
		}
		if note != "" {
			form["note"] = []string{note}
		}
		req := Request{Method: http.MethodPost, Path: SearchPrefix}
		switch path {
		case "template":
			for _, k := range []string{"files", "entry", "args", "dataset_ids", "timeout_minutes"} {
				if v, ok := a[k]; ok && v != nil {
					return Request{}, fmt.Errorf("%s belongs to path notebook", k)
				}
			}
			id, ok, err := a.int("base_container_id")
			if err != nil {
				return Request{}, err
			}
			if !ok || id <= 0 {
				return Request{}, errors.New("base_container_id missing")
			}
			form["base_container_id"] = []string{fmt.Sprint(id)}
		case "notebook":
			if v, ok := a["base_container_id"]; ok && v != nil {
				return Request{}, errors.New("base_container_id belongs to path template")
			}
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
			run := RunBody{Entry: entry, Args: args, TimeoutMinutes: NotebookTimeoutMinutes}
			for _, v := range ids {
				var id int64
				if _, err := fmt.Sscan(strings.TrimSpace(v), &id); err != nil || id < 0 {
					return Request{}, fmt.Errorf("dataset_ids: %q is not a dataset id", v)
				}
				run.DatasetIDs = append(run.DatasetIDs, id)
			}
			if minutes, has, err := a.int("timeout_minutes"); err != nil {
				return Request{}, err
			} else if has {
				if minutes < 1 {
					return Request{}, errors.New("timeout_minutes must be positive")
				}
				run.TimeoutMinutes = int(minutes)
			}
			parsed, err := ParseRunForm(RunForm(run), NotebookTimeoutMax)
			if err != nil {
				return Request{}, err
			}
			names := make([]string, 0, len(paths))
			for _, p := range paths {
				n := baseName(p)
				if n == "params.json" {
					return Request{}, errors.New("params.json is not one of the fixed files: you write it for every trial, with the params you declare with {{tool:search_next}}")
				}
				names = append(names, n)
				req.Files = append(req.Files, File{Field: "file", Path: p})
			}
			if err := ValidateRunFiles(parsed, names, nil); err != nil {
				return Request{}, err
			}
			for k, v := range RunForm(parsed) {
				form[k] = v
			}
		default:
			return Request{}, errors.New(`path must be "template" or "notebook"`)
		}
		req.Form = form
		return req, nil
	},
}

var searchNext = Tool{
	Name: "search_next", CLI: "search-next",
	Desc: "Declares the next trial of a running training search: params must name every parameter of the search space and stay inside it; " +
		"note says in one or two sentences why this trial (the user sees it as your note). " +
		"Then make exactly the calls the answer names; they need no approval.",
	Params: []Param{
		{Name: "search_id", Type: "string", Required: true, Desc: "id of the search, from {{tool:propose_search}}"},
		{Name: "params", Type: "object", Required: true, Desc: "a value for every parameter of the search space"},
		{Name: "note", Type: "string", Desc: "one or two sentences: why this trial (at most 500 characters)"},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		id, err := searchID(a)
		if err != nil {
			return Request{}, err
		}
		params, ok, err := a.object("params")
		if err != nil {
			return Request{}, err
		}
		if !ok {
			return Request{}, errors.New("params missing")
		}
		note, err := searchNote(a)
		if err != nil {
			return Request{}, err
		}
		body, err := json.Marshal(struct {
			Params json.RawMessage `json:"params"`
			Note   string          `json:"note"`
		}{params, note})
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: SearchPrefix + "/" + id + "/trials", Body: body}, nil
	},
}

var searchStatus = Tool{
	Name: "search_status", CLI: "search-status",
	Desc: "Shows a training search: budget used and left, every trial with its parameters, validation value and state, " +
		"the best trial and the user's hints. Read it before you choose the next trial; never repeat a trial.",
	Params: []Param{{Name: "search_id", Type: "string", Required: true, Desc: "id of the search, from {{tool:propose_search}}"}},
	build: func(a Args) (Request, error) {
		id, err := searchID(a)
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodGet, Path: SearchPrefix + "/" + id}, nil
	},
}

var endSearch = Tool{
	Name: "end_search", CLI: "end-search",
	Desc: "Ends a running training search early, e.g. when further trials make no sense. The winner is the best finished trial.",
	Params: []Param{
		{Name: "search_id", Type: "string", Required: true, Desc: "id of the search"},
		{Name: "note", Type: "string", Desc: "why you end it (at most 500 characters)"},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		id, err := searchID(a)
		if err != nil {
			return Request{}, err
		}
		note, err := searchNote(a)
		if err != nil {
			return Request{}, err
		}
		body, err := json.Marshal(struct {
			Note string `json:"note"`
		}{note})
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: SearchPrefix + "/" + id + "/end", Body: body}, nil
	},
}

var cloneTraining = Tool{
	Name: "clone_training", CLI: "clone-training",
	Desc: "Creates a copy of a built training container with other train_config values, without building a new image (effect write). " +
		"Inside a training search use the values {{tool:search_next}} names.",
	Params: []Param{
		{Name: "container_id", Type: "integer", Required: true, Desc: "id of the built training container to copy"},
		{Name: "train_config", Type: "object", Required: true, Desc: "the configuration of the copy: the values of the base with the changes"},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		id, ok, err := a.int("container_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || id <= 0 {
			return Request{}, errors.New("container_id missing")
		}
		cfg, ok, err := a.object("train_config")
		if err != nil {
			return Request{}, err
		}
		if !ok {
			return Request{}, errors.New("train_config missing")
		}
		body, err := json.Marshal(struct {
			TrainConfig json.RawMessage `json:"train_config"`
		}{cfg})
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: fmt.Sprintf("/train/containers/%d/clone", id), Body: body}, nil
	},
}

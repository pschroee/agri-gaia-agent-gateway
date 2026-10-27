// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func buildTool(t *testing.T, name, args string) (Request, error) {
	t.Helper()
	tool, ok := Lookup(name)
	if !ok {
		t.Fatalf("tool %s missing", name)
	}
	return tool.Build(json.RawMessage(args))
}

const searchSpaceArg = `{"learning-rate":{"type":"float","min":0.0001,"max":0.1,"scale":"log"}}`

func TestSearchToolsRegistered(t *testing.T) {
	for _, n := range []string{"propose_search", "search_next", "search_status", "end_search", "clone_training", "training_curves"} {
		tool, ok := Lookup(n)
		if !ok || tool.Desc == "" {
			t.Errorf("%s is not in the table", n)
		}
		if _, ok := Lookup("platform_" + n); !ok {
			t.Errorf("%s is not found with its MCP prefix", n)
		}
	}
	if tool, _ := Lookup("search-next"); tool.Name != "search_next" {
		t.Error("the CLI name does not find the tool")
	}
}

func TestProposeSearchTemplate(t *testing.T) {
	r, err := buildTool(t, "propose_search", `{"title":"Learning rate","path":"template","base_container_id":63,"space":`+searchSpaceArg+
		`,"budget":{"max_runs":3},"note":"start wide"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.Path != SearchPrefix || len(r.Files) != 0 {
		t.Fatalf("request: %+v", r)
	}
	f := r.Form
	if f["title"][0] != "Learning rate" || f["path"][0] != "template" || f["base_container_id"][0] != "63" || f["note"][0] != "start wide" {
		t.Errorf("form: %v", f)
	}
	var space map[string]any
	if json.Unmarshal([]byte(f["space"][0]), &space) != nil || space["learning-rate"] == nil {
		t.Errorf("space: %s", f["space"][0])
	}
	if f["budget"][0] != `{"max_runs":3}` {
		t.Errorf("budget: %s", f["budget"][0])
	}
}

func TestProposeSearchNotebook(t *testing.T) {
	r, err := buildTool(t, "propose_search", `{"title":"t","path":"notebook","space":`+searchSpaceArg+
		`,"files":["/workspace/train.py","/workspace/util.py"],"entry":"train.py","args":["--x=1"],"dataset_ids":["3"],"timeout_minutes":60}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Files) != 2 || r.Files[0].Field != "file" || r.Files[0].Path != "/workspace/train.py" {
		t.Fatalf("files: %+v", r.Files)
	}
	if r.Form["entry"][0] != "train.py" || r.Form["timeout_minutes"][0] != "60" || r.Form["dataset_ids"][0] != "3" {
		t.Errorf("form: %v", r.Form)
	}
}

func TestProposeSearchRefuses(t *testing.T) {
	cases := []struct{ name, args, want string }{
		{"no space", `{"title":"t","path":"template","base_container_id":1}`, "space"},
		{"bad path", `{"title":"t","path":"cloud","space":` + searchSpaceArg + `}`, "path must be"},
		{"template without a container", `{"title":"t","path":"template","space":` + searchSpaceArg + `}`, "base_container_id"},
		{"template with files", `{"title":"t","path":"template","base_container_id":1,"files":["/a.py"],"space":` + searchSpaceArg + `}`, "belongs to path notebook"},
		{"notebook with a container", `{"title":"t","path":"notebook","base_container_id":1,"files":["/a.py"],"entry":"a.py","space":` + searchSpaceArg + `}`, "belongs to path template"},
		{"notebook without files", `{"title":"t","path":"notebook","entry":"a.py","space":` + searchSpaceArg + `}`, "files"},
		{"params.json among the files", `{"title":"t","path":"notebook","files":["/a.py","/w/params.json"],"entry":"a.py","space":` + searchSpaceArg + `}`, "params.json"},
		{"entry not among the files", `{"title":"t","path":"notebook","files":["/a.py"],"entry":"b.py","space":` + searchSpaceArg + `}`, "entry"},
		{"data file", `{"title":"t","path":"notebook","files":["/a.py","/w/x.png"],"entry":"a.py","space":` + searchSpaceArg + `}`, "allowed are"},
		{"long note", `{"title":"t","path":"template","base_container_id":1,"space":` + searchSpaceArg + `,"note":"` + strings.Repeat("x", SearchNoteMax+1) + `"}`, "note"},
		{"space not an object", `{"title":"t","path":"template","base_container_id":1,"space":"[1]"}`, "space"},
	}
	for _, c := range cases {
		if _, err := buildTool(t, "propose_search", c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", c.name, err, c.want)
		}
	}
}

func TestSearchNextStatusEnd(t *testing.T) {
	r, err := buildTool(t, "search_next", `{"search_id":"s0123456789ab","params":{"learning-rate":0.01},"note":"lower"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.Path != SearchPrefix+"/s0123456789ab/trials" {
		t.Fatalf("search_next: %+v", r)
	}
	var body struct {
		Params map[string]float64 `json:"params"`
		Note   string             `json:"note"`
	}
	if json.Unmarshal(r.Body, &body) != nil || body.Params["learning-rate"] != 0.01 || body.Note != "lower" {
		t.Errorf("body: %s", r.Body)
	}
	if r, err := buildTool(t, "search_status", `{"search_id":"s0123456789ab"}`); err != nil || r.Method != "GET" || r.Path != SearchPrefix+"/s0123456789ab" {
		t.Errorf("search_status: %+v %v", r, err)
	}
	if r, err := buildTool(t, "end_search", `{"search_id":"s0123456789ab","note":"enough"}`); err != nil || r.Path != SearchPrefix+"/s0123456789ab/end" || string(r.Body) != `{"note":"enough"}` {
		t.Errorf("end_search: %+v %v", r, err)
	}
	for _, bad := range []string{`{"search_id":"../notebook","params":{}}`, `{"search_id":"S0123456789AB","params":{}}`, `{"search_id":"s0123456789ab/trials","params":{}}`} {
		if _, err := buildTool(t, "search_next", bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := buildTool(t, "search_next", `{"search_id":"s0123456789ab"}`); err == nil {
		t.Error("search_next without params accepted")
	}
}

func TestCloneTraining(t *testing.T) {
	r, err := buildTool(t, "clone_training", `{"container_id":63,"train_config":{"learning-rate":0.01,"epochs":10}}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.Path != "/train/containers/63/clone" || !strings.Contains(string(r.Body), `"learning-rate":0.01`) {
		t.Fatalf("clone: %+v %s", r, r.Body)
	}
	// The configuration reaches a shell in the training container: the check of create_training applies.
	if _, err := buildTool(t, "clone_training", `{"container_id":63,"train_config":{"learning-rate":"1; rm -rf /"}}`); err == nil {
		t.Error("shell code in the configuration of a copy was accepted")
	}
	if _, err := buildTool(t, "clone_training", `{"container_id":63,"train_config":{"a b":1}}`); err == nil {
		t.Error("a key with a space was accepted")
	}
	if _, err := buildTool(t, "clone_training", `{"train_config":{}}`); err == nil {
		t.Error("clone without a container accepted")
	}
}

// A raw call to the search paths is refused: only the search tools may go there.
func TestSearchPathsRefusedForRawCalls(t *testing.T) {
	for _, p := range []string{SearchPrefix, SearchPrefix + "/s0123456789ab/trials", "/_AGW/Search/s0123456789ab"} {
		_, err := buildTool(t, "request", `{"method":"POST","path":"`+p+`","body":{}}`)
		if err == nil || !strings.Contains(err.Error(), "search tools") {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// training_curves reads the per-epoch figures through the virtual path; exactly one of the two arguments (#248).
func TestTrainingCurvesBuild(t *testing.T) {
	r, err := buildTool(t, "training_curves", `{"train_container_id":145}`)
	if err != nil || r.Method != "GET" || r.Path != CurvesPath || r.Query["train_container_id"] != "145" || len(r.Query) != 1 {
		t.Errorf("by container: %+v %v", r, err)
	}
	r, err = buildTool(t, "training_curves", `{"search_id":"s0123456789ab"}`)
	if err != nil || r.Path != CurvesPath || r.Query["search_id"] != "s0123456789ab" || len(r.Query) != 1 {
		t.Errorf("by search: %+v %v", r, err)
	}
	for _, bad := range []string{`{}`, `{"train_container_id":145,"search_id":"s0123456789ab"}`, `{"search_id":"x"}`, `{"train_container_id":0}`} {
		if _, err := buildTool(t, "training_curves", bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	tool, _ := Lookup("training_curves")
	if !strings.Contains(tool.Desc, "never the held-out set") {
		t.Errorf("description: %s", tool.Desc)
	}
	if SearchCallRefusal(Request{Method: "GET", Path: CurvesPath}) == "" {
		t.Error("a raw call to the curves path must be refused")
	}
}

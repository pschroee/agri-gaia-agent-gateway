// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGatewayIndexLines(t *testing.T) {
	idx := GatewayIndex()
	lines := strings.Split(strings.TrimRight(idx, "\n"), "\n")
	tools := gatewayTools()
	if len(lines) != len(tools)+1 {
		t.Fatalf("%d lines for %d tools", len(lines), len(tools))
	}
	for i, tool := range tools {
		line := lines[i]
		c := tool.RESTCall()
		if !strings.HasPrefix(line, "  "+c.Method+" "+c.Path+" — ") || strings.Contains(line, " · tool ") {
			t.Errorf("%s: line %q", tool.Name, line)
		}
		for _, a := range tool.restArgs(c) {
			if a.Where == "body" && c.Body == "" && !strings.Contains(line, a.label()) {
				t.Errorf("%s: body field %s missing", tool.Name, a.label())
			}
			if a.Where == "path" && strings.Contains(line, " body: "+a.label()) {
				t.Errorf("%s: path argument %s listed as body field", tool.Name, a.label())
			}
		}
		for _, p := range tool.Params {
			if p.Path && strings.Contains(strings.SplitN(line, " · ", 2)[len(strings.SplitN(line, " · ", 2))-1], p.Name+"*") {
				t.Errorf("%s: sandbox argument %s listed", tool.Name, p.Name)
			}
		}
	}
	if !strings.Contains(idx, "  POST /_agw/search/{search_id}/trials — ") {
		t.Error("search_next line missing")
	}
	for _, name := range []string{"notebook_run", "notebook_status", "notebook_fetch"} {
		if strings.Contains(idx, "/_agw/"+strings.SplitN(name, "_", 2)[0]) {
			t.Errorf("%s must have no line (sandbox files)", name)
		}
	}
	if strings.Contains(idx, "\x1b") || !strings.HasSuffix(lines[len(lines)-1], " gateway paths)") {
		t.Errorf("closing line: %q", lines[len(lines)-1])
	}
	if len(idx) > 24<<10 {
		t.Errorf("gateway index is %d bytes, limit 24 KiB", len(idx))
	}
	t.Logf("gateway index: %d bytes, %d paths", len(idx), len(tools))
}

// restCase is one call of the REST binding and the arguments of the tool it must be equal to.
type restCase struct {
	method, path string
	query        map[string]string
	body         string
	args         string
}

const (
	cUUID   = "11111111-2222-3333-4444-555555555555"
	cSearch = "s0123456789ab"
	cFn     = "agw-test-m1-abcd1234"
)

var restCases = map[string]restCase{
	"propose_search": {"POST", "/_agw/search", nil, `{"title":"lr","path":"template","space":{"lr":{"type":"float","min":0.0001,"max":0.1,"scale":"log"}},"base_container_id":3,"note":"n"}`,
		`{"title":"lr","path":"template","space":{"lr":{"type":"float","min":0.0001,"max":0.1,"scale":"log"}},"base_container_id":3,"note":"n"}`},
	"search_next":   {"POST", "/_agw/search/" + cSearch + "/trials", nil, `{"params":{"lr":0.01},"note":"x"}`, `{"search_id":"` + cSearch + `","params":{"lr":0.01},"note":"x"}`},
	"search_status": {"GET", "/_agw/search/" + cSearch, nil, ``, `{"search_id":"` + cSearch + `"}`},
	"end_search":    {"POST", "/_agw/search/" + cSearch + "/end", nil, `{"note":"done"}`, `{"search_id":"` + cSearch + `","note":"done"}`},

	"assess_status":    {"GET", "/_agw/assess/checks/" + cUUID, nil, ``, `{"check_id":"` + cUUID + `"}`},
	"split_dataset":    {"POST", "/_agw/assess/splits", nil, `{"dataset_id":5,"train_share":80}`, `{"dataset_id":5,"train_share":80}`},
	"assess_model":     {"POST", "/_agw/assess/checks", nil, `{"model_id":4,"final":true}`, `{"model_id":4,"final":true}`},
	"deployment_check": {"GET", "/_agw/assess/deployments/7", map[string]string{"model_id": "3"}, ``, `{"deployment_id":7,"model_id":3}`},

	"annotation_functions": {"GET", "/_agw/annotation/functions", nil, ``, ``},
	"annotation_function":  {"POST", "/_agw/annotation/functions", nil, `{"model_id":9,"kind":"classification","labels":["a","b"]}`, `{"model_id":9,"kind":"classification","labels":["a","b"]}`},
	"annotation_task":      {"POST", "/_agw/annotation/tasks", nil, `{"dataset_id":5}`, `{"dataset_id":5}`},
	"pre_annotate": {"POST", "/_agw/annotation/runs", nil, `{"dataset_id":5,"function":"` + cFn + `","mapping":{"class_0":"0"},"existing":"keep"}`,
		`{"dataset_id":5,"function":"` + cFn + `","mapping":{"class_0":"0"},"existing":"keep"}`},
	"annotation_run":   {"GET", "/_agw/annotation/runs/" + cUUID, nil, ``, `{"run_id":"` + cUUID + `"}`},
	"annotation_check": {"POST", "/_agw/annotation/checks", nil, `{"mode":"calibrate","dataset_id":5,"function":"` + cFn + `"}`, `{"mode":"calibrate","dataset_id":5,"function":"` + cFn + `"}`},
	"annotation_fill": {"POST", "/_agw/annotation/fills", nil, `{"dataset_id":5,"function":"` + cFn + `","mapping":{"class_0":"0"}}`,
		`{"dataset_id":5,"function":"` + cFn + `","mapping":{"class_0":"0"}}`},
	"annotation_job":      {"GET", "/_agw/annotation/jobs/" + cUUID, map[string]string{"page": "2"}, ``, `{"job_id":"` + cUUID + `","page":2}`},
	"annotation_rollback": {"POST", "/_agw/annotation/rollbacks", nil, `{"source_id":"run-1","scope":"all"}`, `{"source_id":"run-1","scope":"all"}`},
	"annotation_summary":  {"GET", "/_agw/annotation/datasets/5/summary", nil, ``, `{"dataset_id":5}`},
	"annotation_sync":     {"POST", "/_agw/annotation/datasets/5/sync", nil, ``, `{"dataset_id":5}`},
}

// The REST call on a gateway path builds exactly the request of the tool (#199): same method, path, query,
// body and form. A tool with a path but no row here fails the test.
func TestGatewayCallMatchesTool(t *testing.T) {
	for _, tool := range gatewayTools() {
		c := tool.RESTCall()
		if c.Raw {
			got, args, why := GatewayCall(c.Method, c.Path, nil, nil)
			if why != "" || args != nil || got.Name != tool.Name {
				t.Errorf("%s: pass-through path: %q %v %s", tool.Name, why, args, got.Name)
			}
			continue
		}
		rc, ok := restCases[tool.Name]
		if !ok {
			t.Errorf("%s has a gateway path but no case in restCases", tool.Name)
			continue
		}
		if rc.method != c.Method {
			t.Errorf("%s: case method %s, table %s", tool.Name, rc.method, c.Method)
		}
		got, args, why := GatewayCall(rc.method, rc.path, rc.query, []byte(rc.body))
		if why != "" || got.Name != tool.Name {
			t.Errorf("%s: GatewayCall: %q (tool %s)", tool.Name, why, got.Name)
			continue
		}
		fromREST, err := got.Build(args)
		if err != nil {
			t.Errorf("%s: Build of the mapped arguments %s: %v", tool.Name, args, err)
			continue
		}
		direct, err := tool.Build(json.RawMessage(rc.args))
		if err != nil {
			t.Errorf("%s: Build of the tool's arguments: %v", tool.Name, err)
			continue
		}
		if !reflect.DeepEqual(fromREST, direct) {
			t.Errorf("%s: REST call builds\n%+v\nthe tool builds\n%+v", tool.Name, fromREST, direct)
		}
	}
}

func TestGatewayCallRefuses(t *testing.T) {
	for _, c := range []struct {
		name, method, path string
		query              map[string]string
		body               string
		want               string
	}{
		{"unknown path", "GET", "/_agw/nothing", nil, ``, "not a gateway path"},
		{"wrong method", "DELETE", "/_agw/search/" + cSearch, nil, ``, "not a gateway path"},
		{"notebook", "POST", "/_agw/notebook/runs", nil, `{}`, "notebook calls go through the notebook tools"},
		{"assess read path", "GET", "/_agw/assess/models/5/latest", nil, ``, "not a gateway path"},
		{"assess without id", "GET", "/_agw/assess/checks", nil, ``, "not a gateway path"},
		{"sandbox file", "POST", "/_agw/assess/checks", nil, `{"path":"/workspace/m.onnx"}`, "path is not available in the REST binding"},
		{"skipped field", "POST", "/_agw/assess/checks", nil, `{"model_id":4,"preprocess":{}}`, "preprocess is not available in the REST binding"},
		{"notebook field of a search", "POST", "/_agw/search", nil, `{"title":"t","path":"notebook","entry":"train.py"}`, "entry is not available in the REST binding"},
		{"foreign query", "GET", "/_agw/search/" + cSearch, map[string]string{"x": "1"}, ``, `query parameter "x" is not one of`},
		{"duplicate key", "POST", "/_agw/assess/splits", nil, `{"dataset_id":5,"dataset_id":6}`, `field "dataset_id" given twice`},
		{"path argument in the body", "POST", "/_agw/search/" + cSearch + "/end", nil, `{"search_id":"s999999999999"}`, "search_id belongs in the path"},
		{"not a number", "POST", "/_agw/annotation/datasets/abc/sync", nil, ``, "dataset_id must be a whole number"},
		{"body not an object", "POST", "/_agw/assess/splits", nil, `[1]`, "JSON object"},
		{"GET with a body", "GET", "/_agw/search/" + cSearch, nil, `{}`, "GET takes no body"},
	} {
		_, _, why := GatewayCall(c.method, c.path, c.query, []byte(c.body))
		if !strings.Contains(why, c.want) {
			t.Errorf("%s: refusal %q, want %q", c.name, why, c.want)
		}
	}
	if RESTRefusal("GET", "/_agw/search/"+cSearch) != "" {
		t.Error("a path with a tool must not be refused")
	}
}

func TestGatewayIndexDetail(t *testing.T) {
	d := GatewayIndexDetail("/_agw/search")
	if !strings.Contains(d, "search_id* (path, string) — id of the search") {
		t.Errorf("detail of /_agw/search:\n%s", d)
	}
	if strings.Contains(d, "annotation") || strings.Contains(d, "entry (") {
		t.Errorf("detail leaks other areas or skipped arguments:\n%s", d)
	}
	if got := GatewayIndexDetail("/_agw/nothing"); !strings.HasPrefix(got, "no gateway path starts with /_agw/nothing") {
		t.Errorf("unknown prefix: %q", got)
	}
}

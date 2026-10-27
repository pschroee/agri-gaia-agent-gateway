// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The gateway's own paths (/_agw/…) in the REST binding (#199). A path there is the tool of the same
// name: the socket maps method, path, query and JSON body onto the tool's arguments and builds the call
// with Tool.Build, so the REST binding gets the same checks, the same request and the same approval as
// the command line and MCP. The table below says which path belongs to which tool; it also produces the
// "Gateway paths" block of the system note.

// RESTCall is how the REST binding calls a gateway tool. Path has {param} placeholders (the names of the
// tool's parameters), Query names the parameters read from the query, Skip the parameters REST cannot pass
// (besides those with Param.Path, which need files of the sandbox). Raw: the call is passed through as it
// is (rights, capabilities, plan). Body replaces the generated description of the body, Desc the tool's
// description, Note is added to the line, Example is a path with sample values (tests).
type RESTCall struct {
	Method, Path string
	Query, Skip  []string
	Raw          bool
	Body, Desc   string
	Note         string
	Example      string
}

const (
	restUUID   = "11111111-2222-3333-4444-555555555555"
	restSearch = "s0123456789ab"
)

// restCalls is the table of gateway tools that have a path in the REST binding, by tool name. The notebook
// tools are not in it: a run gets files of the sandbox, which the REST binding does not have.
var restCalls = map[string]RESTCall{
	"rights":       {Method: "GET", Path: "/_agw/rights", Raw: true, Example: "/_agw/rights"},
	"capabilities": {Method: "GET", Path: "/_agw/capabilities", Raw: true, Example: "/_agw/capabilities"},
	"propose_plan": {Method: "POST", Path: "/_agw/plan", Raw: true, Example: "/_agw/plan",
		Body: "the plan: title*, steps*, mode, budget, replaces"},
	"api_paths": {Method: "GET", Path: "/_agw/paths", Query: []string{"prefix"}, Raw: true, Example: "/_agw/paths",
		Desc: "Index of all paths of the platform's REST API, one line per operation. With query prefix only paths starting with it (e.g. /train). With prefix=/_agw/<area> (e.g. /_agw/search) it describes each argument of the gateway paths of that area."},

	"propose_search": {Method: "POST", Path: "/_agw/search", Skip: []string{"entry", "args", "dataset_ids", "timeout_minutes"},
		Note: `path "notebook" needs files of the sandbox and is not available here; use path "template"`, Example: "/_agw/search"},
	"search_next":   {Method: "POST", Path: "/_agw/search/{search_id}/trials", Example: "/_agw/search/" + restSearch + "/trials"},
	"search_status": {Method: "GET", Path: "/_agw/search/{search_id}", Example: "/_agw/search/" + restSearch},
	"end_search":    {Method: "POST", Path: "/_agw/search/{search_id}/end", Example: "/_agw/search/" + restSearch + "/end"},
	"training_curves": {Method: "GET", Path: "/_agw/curves", Query: []string{"train_container_id", "search_id"},
		Example: "/_agw/curves"},

	"assess_status": {Method: "GET", Path: "/_agw/assess/checks/{check_id}", Example: "/_agw/assess/checks/" + restUUID},
	"split_dataset": {Method: "POST", Path: "/_agw/assess/splits", Example: "/_agw/assess/splits"},
	"assess_model": {Method: "POST", Path: "/_agw/assess/checks", Skip: []string{"class_names", "preprocess"},
		Note: "checks a platform model (model_id); a file of the sandbox (path) is not available here", Example: "/_agw/assess/checks"},
	"deployment_check": {Method: "GET", Path: "/_agw/assess/deployments/{deployment_id}", Query: []string{"model_id"},
		Example: "/_agw/assess/deployments/7"},

	"annotation_functions": {Method: "GET", Path: "/_agw/annotation/functions", Example: "/_agw/annotation/functions"},
	"annotation_function":  {Method: "POST", Path: "/_agw/annotation/functions", Example: "/_agw/annotation/functions"},
	"annotation_task":      {Method: "POST", Path: "/_agw/annotation/tasks", Example: "/_agw/annotation/tasks"},
	"pre_annotate":         {Method: "POST", Path: "/_agw/annotation/runs", Example: "/_agw/annotation/runs"},
	"annotation_run":       {Method: "GET", Path: "/_agw/annotation/runs/{run_id}", Example: "/_agw/annotation/runs/" + restUUID},
	"annotation_check":     {Method: "POST", Path: "/_agw/annotation/checks", Example: "/_agw/annotation/checks"},
	"annotation_fill":      {Method: "POST", Path: "/_agw/annotation/fills", Example: "/_agw/annotation/fills"},
	"annotation_job":       {Method: "GET", Path: "/_agw/annotation/jobs/{job_id}", Query: []string{"page"}, Example: "/_agw/annotation/jobs/" + restUUID},
	"annotation_rollback":  {Method: "POST", Path: "/_agw/annotation/rollbacks", Example: "/_agw/annotation/rollbacks"},
	"annotation_summary":   {Method: "GET", Path: "/_agw/annotation/datasets/{dataset_id}/summary", Example: "/_agw/annotation/datasets/5/summary"},
	"annotation_sync":      {Method: "POST", Path: "/_agw/annotation/datasets/{dataset_id}/sync", Example: "/_agw/annotation/datasets/5/sync"},
}

// RESTCall returns the REST description of the tool, nil if the tool has no gateway path.
func (t Tool) RESTCall() *RESTCall {
	c, ok := restCalls[t.Name]
	if !ok {
		return nil
	}
	return &c
}

// restArg is one argument of a gateway tool as the REST binding passes it.
type restArg struct {
	Param Param
	Where string // path, query or body
}

// restArgs splits the parameters of the tool by where the REST call carries them. Skipped parameters and
// those that need sandbox files are left out.
func (t Tool) restArgs(c *RESTCall) []restArg {
	var out []restArg
	for _, p := range t.Params {
		switch {
		case strings.Contains(c.Path, "{"+p.Name+"}"):
			out = append(out, restArg{p, "path"})
		case contains(c.Query, p.Name):
			out = append(out, restArg{p, "query"})
		case p.Path || contains(c.Skip, p.Name):
		default:
			out = append(out, restArg{p, "body"})
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (a restArg) label() string {
	if a.Param.Required {
		return a.Param.Name + "*"
	}
	return a.Param.Name
}

// restLine is the line of the tool in the index.
func (t Tool) restLine(c *RESTCall) string {
	desc := c.Desc
	if desc == "" {
		desc = t.Desc
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — %s", c.Method, c.Path, VisibleControls(oneLine(desc)))
	var q, body []string
	for _, a := range t.restArgs(c) {
		switch a.Where {
		case "query":
			q = append(q, a.label())
		case "body":
			body = append(body, a.label())
		}
	}
	if len(q) > 0 {
		fmt.Fprintf(&b, " · query: %s", strings.Join(q, ", "))
	}
	switch {
	case c.Body != "":
		fmt.Fprintf(&b, " · body: %s", c.Body)
	case len(body) > 0:
		fmt.Fprintf(&b, " · body: %s", strings.Join(body, ", "))
	}
	if c.Note != "" {
		fmt.Fprintf(&b, " · here: %s", c.Note)
	}
	return b.String()
}

// gatewayTools lists the tools with a gateway path in the order of the table of tools.
func gatewayTools() []Tool {
	var out []Tool
	for _, t := range Tools {
		if t.RESTCall() != nil {
			out = append(out, t)
		}
	}
	return out
}

// GatewayIndex is the block "Gateway paths" of the system note of the REST binding: one line per tool with
// a gateway path (method, path, what it does, query and body fields), every line with two spaces of indent,
// then the count. The descriptions carry tool references; the caller renders them (Render).
func GatewayIndex() string {
	var b strings.Builder
	n := 0
	for _, t := range gatewayTools() {
		b.WriteString("  " + t.restLine(t.RESTCall()) + "\n")
		n++
	}
	fmt.Fprintf(&b, "  (%d gateway paths)\n", n)
	return b.String()
}

// GatewayIndexDetail answers GET /_agw/paths with prefix=/_agw/…: the lines of the paths that start with the
// prefix and under each line every argument with its description.
func GatewayIndexDetail(prefix string) string {
	var b strings.Builder
	for _, t := range gatewayTools() {
		c := t.RESTCall()
		if !strings.HasPrefix(c.Path, prefix) {
			continue
		}
		b.WriteString(t.restLine(c) + "\n")
		for _, a := range t.restArgs(c) {
			typ := a.Param.Type
			if typ == "" {
				typ = "JSON"
			}
			fmt.Fprintf(&b, "    %s (%s, %s) — %s\n", a.label(), a.Where, typ, VisibleControls(oneLine(a.Param.Desc)))
			if sj := a.Param.SchemaJSON(); sj != "" {
				fmt.Fprintf(&b, "      schema: %s\n", VisibleControls(sj))
			}
		}
		if c.Body != "" {
			fmt.Fprintf(&b, "    body: %s\n", c.Body)
		}
	}
	if b.Len() == 0 {
		return "no gateway path starts with " + VisibleControls(prefix) + "\n"
	}
	return b.String()
}

// RESTHint is appended to the message when Tool.Build refuses the arguments of a REST call.
const RESTHint = " (REST: path, query and body fields as in the system note under Gateway paths)"

// GatewayCall maps a REST call on a gateway path (/_agw/…) onto its tool and the tool's arguments. The
// third value is the reason for a refusal, empty otherwise. For a pass-through path (rights, capabilities,
// plan, paths) the arguments are nil and the caller sends the call as it is.
func GatewayCall(method, path string, query map[string]string, body []byte) (Tool, json.RawMessage, string) {
	if why := NotebookCallRefusal(Request{Path: path}); why != "" {
		return Tool{}, nil, why
	}
	t, c, vals, ok := matchGateway(method, path)
	if !ok {
		return Tool{}, nil, fmt.Sprintf("%s %s is not a gateway path of the REST binding; the system note lists them under Gateway paths", method, path)
	}
	if c.Raw {
		return t, nil, ""
	}
	args := map[string]json.RawMessage{}
	byName := map[string]restArg{}
	for _, a := range t.restArgs(c) {
		byName[a.Param.Name] = a
	}
	set := func(a restArg, raw string) string {
		if a.Param.Type == "integer" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || strings.Trim(raw, "0123456789") != "" {
				return fmt.Sprintf("%s must be a whole number", a.Param.Name)
			}
			args[a.Param.Name] = json.RawMessage(strconv.FormatInt(n, 10))
			return ""
		}
		v, _ := json.Marshal(raw)
		args[a.Param.Name] = v
		return ""
	}
	for name, v := range vals {
		if why := set(byName[name], v); why != "" {
			return t, nil, why
		}
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a, ok := byName[k]
		if !ok || a.Where != "query" {
			return t, nil, fmt.Sprintf("query parameter %q is not one of %s", k, strings.Join(c.Query, ", "))
		}
		if why := set(a, query[k]); why != "" {
			return t, nil, why
		}
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if method == http.MethodGet {
			return t, nil, "GET takes no body"
		}
		d := json.NewDecoder(bytes.NewReader(body))
		if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
			return t, nil, "the body of a gateway path is a JSON object"
		}
		for d.More() {
			kt, err := d.Token()
			key, isStr := kt.(string)
			if err != nil || !isStr {
				return t, nil, "the body is not valid JSON"
			}
			var v json.RawMessage
			if err := d.Decode(&v); err != nil {
				return t, nil, "the body is not valid JSON"
			}
			if a, ok := byName[key]; ok && a.Where != "body" {
				return t, nil, fmt.Sprintf("%s belongs in the %s", key, a.Where)
			}
			if p, ok := paramNamed(t, key); ok && (p.Path || contains(c.Skip, key)) {
				return t, nil, fmt.Sprintf("%s is not available in the REST binding", key)
			}
			if _, dup := args[key]; dup {
				return t, nil, fmt.Sprintf("field %q given twice", key)
			}
			args[key] = v
		}
		if _, err := d.Token(); err != nil || d.More() {
			return t, nil, "the body is not valid JSON"
		}
	}
	out, err := json.Marshal(args)
	if err != nil {
		return t, nil, err.Error()
	}
	return t, out, ""
}

func paramNamed(t Tool, name string) (Param, bool) {
	for _, p := range t.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// matchGateway finds the tool whose path pattern fits the call: same method, same number of segments,
// fixed segments equal, placeholders non-empty. vals are the values of the placeholders by name.
func matchGateway(method, path string) (Tool, *RESTCall, map[string]string, bool) {
	segs := strings.Split(path, "/")
	for _, t := range gatewayTools() {
		c := t.RESTCall()
		if c.Method != method {
			continue
		}
		pat := strings.Split(c.Path, "/")
		if len(pat) != len(segs) {
			continue
		}
		vals := map[string]string{}
		match := true
		for i, p := range pat {
			switch {
			case strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}"):
				if segs[i] == "" {
					match = false
				}
				vals[p[1:len(p)-1]] = segs[i]
			case p != segs[i]:
				match = false
			}
		}
		if match {
			return t, c, vals, true
		}
	}
	return Tool{}, nil, nil, false
}

// RESTRefusal is the reason the REST binding refuses a call on the path, empty if a tool takes it.
func RESTRefusal(method, path string) string {
	_, _, why := GatewayCall(method, path, nil, nil)
	return why
}

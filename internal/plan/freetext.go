// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"agw/internal/platform"
)

// freeTextRule names the fields of one kind of plan step that are free text the agent writes for the user (B23,
// widening B20): the gateway never verifies or acts on them, and the platform only stores or shows them. They are
// no part of the plan match, so "Laeufe" in the plan and "Läufe" in the call are the same step. The list is
// explicit, per step kind: a field that is not listed – names, ids, paths, parameters, labels, files and their
// hashes, budgets – is compared exactly (fail closed). Tool steps matched on their arguments (MatchesTool) have no
// free-text argument today; the tools listed here are matched on the request they build.
type freeTextRule struct {
	tool   string // the tool that builds the call; the test keeps fields and parameters in step
	method string
	path   func(string) bool
	form   bool // the fields are form fields (multipart), else top-level keys of the JSON body
	fields []string
}

func exactPath(p string) func(string) bool { return func(s string) bool { return s == p } }

// searchSubPath: <SearchPrefix>/<id>/<last>, one segment for the id.
func searchSubPath(last string) func(string) bool {
	return func(s string) bool {
		rest, ok := strings.CutPrefix(s, platform.SearchPrefix+"/")
		if !ok {
			return false
		}
		id, tail, ok := strings.Cut(rest, "/")
		return ok && id != "" && tail == last
	}
}

var freeTextRules = []freeTextRule{
	// The dataset's and the model's description: stored as text, decide nothing.
	{tool: "upload_dataset", method: http.MethodPost, path: exactPath("/datasets"), form: true, fields: []string{"description"}},
	{tool: "upload_model", method: http.MethodPost, path: exactPath("/models"), form: true, fields: []string{"description"}},
	// The agent's notes and the search's title (B20): shown as the agent's text, marked as not verified.
	{tool: "propose_search", method: http.MethodPost, path: exactPath(platform.SearchPrefix), form: true, fields: []string{"title", "note"}},
	{tool: "search_next", method: http.MethodPost, path: searchSubPath("trials"), fields: []string{"note"}},
	{tool: "end_search", method: http.MethodPost, path: searchSubPath("end"), fields: []string{"note"}},
	{tool: "notebook_run", method: http.MethodPost, path: exactPath(platform.NotebookPrefix + "/runs"), form: true, fields: []string{"note"}},
}

// freeTextFields gives the free-text fields of a call by its method and resolved path; form tells where they are.
func freeTextFields(method, path string) (fields []string, form bool) {
	for _, r := range freeTextRules {
		if r.method == method && r.path(path) {
			return r.fields, r.form
		}
	}
	return nil, false
}

// sameBodyWithout compares two JSON object bodies without the given top-level keys, in canonical form. Anything
// that is not a JSON object on both sides is compared as it is.
func sameBodyWithout(want, got []byte, drop []string) bool {
	w, e1 := jsonObject(want)
	g, e2 := jsonObject(got)
	if e1 != nil || e2 != nil {
		wc, e1 := Canonical(want)
		gc, e2 := Canonical(got)
		return e1 == nil && e2 == nil && bytes.Equal(wc, gc)
	}
	for _, k := range drop {
		delete(w, k)
		delete(g, k)
	}
	wb, e1 := json.Marshal(w)
	gb, e2 := json.Marshal(g)
	if e1 != nil || e2 != nil {
		return false
	}
	wc, e1 := Canonical(wb)
	gc, e2 := Canonical(gb)
	return e1 == nil && e2 == nil && bytes.Equal(wc, gc)
}

// sameFormWithout compares the form of a step (placeholders resolved) with a live form without the given fields.
func sameFormWithout(stepForm string, live map[string][]string, resolve Resolve, drop []string) bool {
	if strings.TrimSpace(stepForm) == "" {
		stepForm = "{}"
	}
	want, ok := substJSON(stepForm, resolve, false)
	if !ok {
		return false
	}
	var step map[string][]string
	if err := json.Unmarshal([]byte(want), &step); err != nil {
		return false
	}
	got := make(map[string][]string, len(live))
	for k, v := range live {
		got[k] = v
	}
	for _, k := range drop {
		delete(step, k)
		delete(got, k)
	}
	return FormJSON(step) == FormJSON(got)
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"reflect"

	"agw/internal/platform"
	"agw/internal/store"
)

// CallChanged reports whether the call that goes out differs from the one the step stored: another path
// (a placeholder resolved), another query, body or form (a value the tool filled in). Bodies and forms are
// compared as JSON values, so the order of keys does not matter. A stored part that cannot be read counts as
// changed; a false positive only costs one more read for the summary (issue #225).
func CallChanged(st store.PlanStep, req platform.Request) bool {
	if st.Path != req.Path {
		return true
	}
	var q map[string]string
	if len(st.Query) > 0 {
		if err := json.Unmarshal(st.Query, &q); err != nil {
			return true
		}
	}
	if len(q) != len(req.Query) {
		return true
	}
	for k, v := range q {
		if rv, ok := req.Query[k]; !ok || rv != v {
			return true
		}
	}
	if !sameJSON(st.Body, req.Body) {
		return true
	}
	var form json.RawMessage
	if len(req.Form) > 0 {
		b, err := json.Marshal(req.Form)
		if err != nil {
			return true
		}
		form = b
	}
	return !sameJSON(st.Form, form)
}

// sameJSON compares two JSON documents by value; empty equals empty (and "null").
func sameJSON(a, b json.RawMessage) bool {
	va, ok := decodeJSON(a)
	if !ok {
		return false
	}
	vb, ok := decodeJSON(b)
	if !ok {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func decodeJSON(raw json.RawMessage) (any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, true
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

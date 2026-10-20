// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import "agw/internal/platform"

// Change is one object a successful platform call changed (issue #167). The names are those of the
// delegation (resources and actions), so that new routes are covered without a second list.
type Change struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
	ID       string `json:"id,omitempty"`
}

// Changes lists what a successful call changed: the main access and every further access of the call
// whose action is not a read, plus the objects the call created. Reads give an empty list. An unknown
// identifier is left out (""). The caller checks that the call succeeded.
func Changes(a Access, req platform.Request, res platform.Result) []Change {
	var out []Change
	add := func(c Change) {
		if c.ID == unknownID {
			c.ID = ""
		}
		for i, x := range out {
			if x.Resource != c.Resource {
				continue
			}
			if x.ID == c.ID && (x.Action == c.Action || c.Action == Create) {
				return
			}
			if x.ID == "" && c.Action == Create && x.Action == Create {
				out[i].ID = c.ID // the created object's identifier completes the entry
				return
			}
		}
		out = append(out, c)
	}
	if a.Action != Read && a.Resource != "" {
		add(Change{Resource: a.Resource, Action: a.Action, ID: a.ID})
	}
	for _, x := range a.Also {
		if x.Action != Read && x.Resource != "" {
			add(Change{Resource: x.Resource, Action: x.Action, ID: x.ID})
		}
	}
	if r, id, ok := Created(a, res); ok {
		add(Change{Resource: r, Action: Create, ID: id})
	}
	if ref, ok := CreatedImage(a, req, res); ok {
		add(Change{Resource: ContainerImage, Action: Create, ID: ref})
	}
	return out
}

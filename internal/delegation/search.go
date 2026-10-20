// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"regexp"

	"agw/internal/platform"
)

// The routes of the training search on the platform side (issue #168): the copy of a built training container
// with another train_config (backend fork, no new image) and the figures a training writes (metrics.jsonl and
// the GPU samples). The search itself lives under /_agw/search and is classified where the search service is
// added. init appends the routes, so that other features can add theirs in parallel.

var cloneRe = regexp.MustCompile(`^/train/containers/([^/]+)/clone$`)

// Search is the resource of the training search's own paths (issue #168): declaring the next trial, reading the
// search and ending it. They are virtual (platform.SearchPrefix) and answered by the gateway itself, like the check
// paths; without a rule for search every one of them is forbidden in a delegation. The proposal itself
// (POST /_agw/search) is the gateway's own bookkeeping like a plan proposal and is not classified: what it makes
// possible, the copy and the start of the trials, is classified call by call (clone, run) and checked as any other.
const Search = "search"

// searchSeg is the shape of the id of a search.
const searchSeg = `s[0-9a-f]{12}`

func init() {
	resources[Search] = true
	searchPrefix := regexp.QuoteMeta(platform.SearchPrefix)
	routes = append(routes,
		r(http.MethodGet, searchPrefix+`/`+searchSeg, Read, Search, 0),
		// Declaring a trial and ending a search change nothing on the platform and need no approval of their own
		// (the chat answers them itself); the class is write so that a read-only delegation does not allow them.
		r(http.MethodPost, searchPrefix+`/`+searchSeg+`/trials`, Update, Search, 0).as(EffectWrite),
		r(http.MethodPost, searchPrefix+`/`+searchSeg+`/end`, Update, Search, 0).as(EffectWrite),
	)
	routes = append(routes,
		// A copy is a new training container: create training, and the base container is read (its image, its
		// dataset and its configuration are what the copy takes over), so a base the delegation does not name is
		// refused, whatever the copy would carry.
		r(http.MethodPost, `/train/containers/`+seg+`/clone`, Create, Training, 0).refine(refineClone),
		r(http.MethodGet, `/train/containers/`+seg+`/metrics`, Read, Training, 1),
	)
	// The body of a copy carries train_config and nothing else: the backend takes the image, the dataset and the
	// owner from the base container, and a field of the agent's must never reach them (like PATCH, Review 5, K1).
	fieldRules = append(fieldRules, struct {
		method string
		re     *regexp.Regexp
		fields []string
	}{http.MethodPost, regexp.MustCompile(`^/train/containers/[^/]+/clone$`), []string{"train_config"}})
	registerConformance(searchCloneCases()...)
	registerConformance(searchPathCases()...)
}

// refineClone adds the base container of a copy to what the call touches.
func refineClone(a *Access, req platform.Request) {
	if m := cloneRe.FindStringSubmatch(req.Path); m != nil {
		a.Also = append(a.Also, Access{Action: Read, Resource: Training, ID: canonID(m[1])})
	}
}

// searchCloneRules: a delegation that may create training containers and read container 7.
const searchCloneRules = `{
  "rules": [
    {"action": "create", "resource": "training"},
    {"action": "read",   "resource": "training", "ids": ["7"]}
  ],
  "confirm": "none"
}`

// searchCloneNoCreateRules: the same without the right to create one — a check that refuses everything must not
// pass as correct (G1.6), and one that lets everything through must not either.
const searchCloneNoCreateRules = `{
  "rules": [
    {"action": "read", "resource": "training", "ids": ["7"]}
  ],
  "confirm": "none"
}`

func searchCloneCases() []Case {
	clone := func(id, body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: "/train/containers/" + id + "/clone", Body: []byte(body)}
	}
	return []Case{
		{Name: "copy a container of this delegation (control)", Req: clone("7", `{"train_config":{"lr":0.01}}`), Allowed: true,
			Group: "control", Rules: searchCloneRules},
		{Name: "read the figures of a container of this delegation (control)", Req: platform.Request{Method: http.MethodGet,
			Path: "/train/containers/7/metrics"}, Allowed: true, Group: "control", Rules: searchCloneRules},
		{Name: "copy another user's container", Req: clone("8", `{"train_config":{"lr":0.01}}`), Group: "object", Rules: searchCloneRules},
		{Name: "copy a container with a different image in the body", Req: clone("7", `{"train_config":{},"image_id":"evil"}`),
			Group: "field", Rules: searchCloneRules},
		{Name: "copy a container with another owner in the body", Req: clone("7", `{"train_config":{},"owner":"someone"}`),
			Group: "field", Rules: searchCloneRules},
		{Name: "copy a container without the right to create one", Req: clone("7", `{"train_config":{}}`), Group: "action",
			Rules: searchCloneNoCreateRules},
		{Name: "read the figures of another user's container", Req: platform.Request{Method: http.MethodGet,
			Path: "/train/containers/8/metrics"}, Group: "object", Rules: searchCloneRules},
	}
}

const searchPathRules = `{"rules":[{"action":"read","resource":"search"},{"action":"update","resource":"search"}],"confirm":"none"}`
const searchReadOnlyRules = `{"rules":[{"action":"read","resource":"search"}],"confirm":"none"}`

func searchPathCases() []Case {
	id := "s0123456789ab"
	next := platform.Request{Method: http.MethodPost, Path: platform.SearchPrefix + "/" + id + "/trials", Body: []byte(`{"params":{"lr":0.01}}`)}
	status := platform.Request{Method: http.MethodGet, Path: platform.SearchPrefix + "/" + id}
	end := platform.Request{Method: http.MethodPost, Path: platform.SearchPrefix + "/" + id + "/end", Body: []byte(`{}`)}
	return []Case{
		{Name: "search status without rule", Req: status, Group: "search"},
		{Name: "search status with rule (control)", Req: status, Allowed: true, Group: "control", Rules: searchPathRules},
		{Name: "declare a trial without rule", Req: next, Group: "search"},
		{Name: "declare a trial with rule (control)", Req: next, Allowed: true, Group: "control", Rules: searchPathRules},
		{Name: "declare a trial with a read-only rule", Req: next, Group: "action", Rules: searchReadOnlyRules},
		{Name: "end a search without rule", Req: end, Group: "search"},
		{Name: "end a search with rule (control)", Req: end, Allowed: true, Group: "control", Rules: searchPathRules},
		{Name: "a path that is no search id", Req: platform.Request{Method: http.MethodGet, Path: platform.SearchPrefix + "/x"}, Group: "search",
			Rules: searchPathRules},
	}
}

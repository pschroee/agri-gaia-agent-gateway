// Package delegation checks platform calls against a chat's delegated rights: which actions on
// which objects the agent may perform for this task. Everything else is a violation. Every call
// is classified from its method and normalized path alone, never from what the agent claims
// (docs/plan-delegation-rest-platform.md, Step 1: Delegation).
package delegation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
)

// Actions.
const (
	Read   = "read"
	Create = "create"
	Update = "update"
	Delete = "delete"
	Run    = "run"
)

// Resources.
const (
	Dataset        = "dataset"
	Model          = "model"
	Training       = "training"
	Task           = "task"
	TrainTemplate  = "train_template"
	EdgeDevice     = "edge_device"
	ContainerImage = "container_image"
	API            = "api"  // all other paths
	Docs           = "docs" // description of the API, always readable
)

// Special identifiers in rules.
const (
	All = "*"   // any object
	Own = "own" // created under this delegation (provenance rule)
)

var (
	actions   = map[string]bool{Read: true, Create: true, Update: true, Delete: true, Run: true}
	resources = map[string]bool{Dataset: true, Model: true, Training: true, Task: true, TrainTemplate: true, EdgeDevice: true, ContainerImage: true, API: true}
)

// Rule allows an action on a resource. Without IDs it only applies to calls without an object
// (lists, creating); IDs names identifiers, "*" or "own".
type Rule struct {
	Action   string   `json:"action"`
	Resource string   `json:"resource"`
	IDs      []string `json:"ids,omitempty"`
}

// Delegation holds a chat's delegated rights.
type Delegation struct {
	Rules     []Rule     `json:"rules"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Enforce: true (default) blocks violations; false only logs them (stage "no protection" of the
	// experiment plan: the same observation point, no boundary).
	Enforce *bool `json:"enforce,omitempty"`
	// Confirm: "writes" (default) additionally asks the user for writing calls, "none" does not.
	Confirm string `json:"confirm,omitempty"`
}

// Parse reads and validates a delegation. Unknown fields are an error, so that a typo does not
// silently grant or withdraw rights.
func Parse(raw []byte) (*Delegation, error) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	var del Delegation
	if err := d.Decode(&del); err != nil {
		return nil, fmt.Errorf("delegation unreadable: %w", err)
	}
	if d.More() {
		return nil, errors.New("delegation: further text follows the object")
	}
	return &del, del.Validate()
}

// Validate checks actions, resources and identifiers and brings identifiers into a canonical form.
func (d *Delegation) Validate() error {
	for i := range d.Rules {
		r := &d.Rules[i]
		if !actions[r.Action] {
			return fmt.Errorf("rule %d: unknown action %q (read, create, update, delete, run)", i+1, r.Action)
		}
		if !resources[r.Resource] {
			return fmt.Errorf("rule %d: unknown resource %q", i+1, r.Resource)
		}
		for j, id := range r.IDs {
			id = strings.TrimSpace(id)
			if id == "" {
				return fmt.Errorf("rule %d: empty identifier", i+1)
			}
			if id == unknownID {
				return fmt.Errorf("rule %d: %q is not an identifier", i+1, id)
			}
			if id != All && id != Own {
				id = canonID(id)
			}
			r.IDs[j] = id
		}
	}
	switch d.Confirm {
	case "", "writes", "none":
	default:
		return fmt.Errorf("confirm must be writes or none, not %q", d.Confirm)
	}
	return nil
}

// Enforcing reports whether violations are blocked.
func (d *Delegation) Enforcing() bool { return d.Enforce == nil || *d.Enforce }

// ConfirmWrites reports whether the user additionally confirms writing calls.
func (d *Delegation) ConfirmWrites() bool { return d.Confirm != "none" }

// Summary describes the rights for the agent and the UI.
func (d *Delegation) Summary() string {
	var b strings.Builder
	mode := "violations are blocked"
	if !d.Enforcing() {
		mode = "violations are only logged"
	}
	fmt.Fprintf(&b, "Delegated rights (%s", mode)
	if d.ExpiresAt != nil {
		fmt.Fprintf(&b, ", valid until %s", d.ExpiresAt.Format(time.RFC3339))
	}
	b.WriteString("):\n")
	if len(d.Rules) == 0 {
		b.WriteString("  none; every platform call is a violation\n")
	}
	for _, r := range d.Rules {
		ids := "without object (lists, creating)"
		if len(r.IDs) > 0 {
			names := make([]string, len(r.IDs))
			for i, id := range r.IDs {
				switch id {
				case All:
					names[i] = "all"
				case Own:
					names[i] = "created in this chat"
				default:
					names[i] = id
				}
			}
			ids = strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "  %s %s: %s\n", r.Action, r.Resource, ids)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Access is the classification of a call. Some calls touch more than one object (Review 5,
// K2 and W1): Also names the further accesses that must be allowed as well. Problem is set
// when the call has to be refused regardless of the rules (e.g. a field outside the allowlist).
type Access struct {
	Action   string   `json:"action"`
	Resource string   `json:"resource"`
	ID       string   `json:"id,omitempty"`
	Also     []Access `json:"also,omitempty"`
	Problem  string   `json:"problem,omitempty"`
}

func (a Access) String() string {
	s := a.Action + " " + a.Resource
	if a.ID != "" {
		s += " " + a.ID
	}
	for _, x := range a.Also {
		s += " + " + x.String()
	}
	return s
}

// unknownID stands for an identifier that is missing from the call or unreadable; no rule names it.
const unknownID = "?"

type route struct {
	method   string
	re       *regexp.Regexp
	action   string
	resource string
	idGroup  int // group holding the identifier, 0: without object
}

func r(method, pattern, action, resource string, idGroup int) route {
	return route{method, regexp.MustCompile("^" + pattern + "$"), action, resource, idGroup}
}

const seg = `([^/]+)`

// routes classifies the platform's paths, fixed ones before those with an identifier (otherwise
// /datasets/keyword would be a dataset "keyword"). Whatever is missing here is api.
var routes = []route{
	r("GET", `/openapi\.json`, Read, Docs, 0),
	r("GET", `/datasets`, Read, Dataset, 0),
	r("GET", `/datasets/keyword`, Read, Dataset, 0),
	r("GET", `/datasets/catalogue`, Read, Dataset, 0),
	r("GET", `/datasets/convert/formats`, Read, Dataset, 0),
	r("POST", `/datasets`, Create, Dataset, 0),
	r("POST", `/datasets/import`, Create, Dataset, 0),
	r("GET", `/datasets/`+seg, Read, Dataset, 1),
	// Writing GET: fetches the annotation from CVAT and writes it to MinIO (Review 5, W5).
	r("GET", `/datasets/`+seg+`/download`, Update, Dataset, 1),
	r("PATCH", `/datasets/`+seg, Update, Dataset, 1),
	r("PATCH", `/datasets/`+seg+`/toggle-public`, Update, Dataset, 1),
	r("POST", `/datasets/`+seg+`/annotate`, Update, Dataset, 1),
	r("DELETE", `/datasets/`+seg, Delete, Dataset, 1),

	r("GET", `/models`, Read, Model, 0),
	r("GET", `/models/keyword`, Read, Model, 0),
	r("POST", `/models`, Create, Model, 0),
	r("GET", `/models/`+seg, Read, Model, 1),
	r("GET", `/models/`+seg+`/download`, Read, Model, 1),
	r("PATCH", `/models/`+seg, Update, Model, 1),
	r("PATCH", `/models/`+seg+`/toggle-public`, Update, Model, 1),
	r("DELETE", `/models/`+seg, Delete, Model, 1),

	r("GET", `/train/providers`, Read, TrainTemplate, 0),
	r("GET", `/train/architectures/`+seg, Read, TrainTemplate, 0),
	r("GET", `/train/config/export`, Read, TrainTemplate, 0),
	r("GET", `/train/config/`+seg+`/`+seg, Read, TrainTemplate, 0),
	r("POST", `/train/config`, Create, Training, 0),
	r("PUT", `/train/config`, Update, Training, 0), // identifier is in the body (container_id)
	r("GET", `/train/containers`, Read, Training, 0),
	r("GET", `/train/containers/`+seg, Read, Training, 1),
	r("GET", `/train/containers/`+seg+`/status`, Read, Training, 1),
	r("GET", `/train/containers/`+seg+`/logs`, Read, Training, 1),
	r("GET", `/train/containers/`+seg+`/config`, Read, Training, 1),
	// Writing GET: creates a model from the container (Review K1 of the platform binding).
	r("GET", `/train/containers/`+seg+`/model`, Create, Model, 0),
	r("POST", `/train/containers/`+seg+`/run`, Run, Training, 1),
	r("POST", `/train/containers/`+seg+`/stop`, Run, Training, 1),
	r("PATCH", `/train/containers/`+seg, Update, Training, 1),
	r("PATCH", `/train/containers/`+seg+`/score`, Update, Training, 1),
	r("DELETE", `/train/containers/`+seg, Delete, Training, 1),

	r("GET", `/tasks`, Read, Task, 0),
	r("GET", `/tasks/`+seg, Read, Task, 1),
	r("DELETE", `/tasks/`+seg, Delete, Task, 1),

	r("GET", `/edge-devices`, Read, EdgeDevice, 0),
	r("POST", `/edge-devices`, Create, EdgeDevice, 0),
	// Writing GET: registers a not yet registered device with Portainer (Review 5, W5).
	r("GET", `/edge-devices/`+seg, Update, EdgeDevice, 1),
	r("DELETE", `/edge-devices/`+seg, Delete, EdgeDevice, 1),
	r("GET", `/edge-devices/`+seg+`/deployments`, Read, EdgeDevice, 1),

	r("GET", `/container-images`, Read, ContainerImage, 0),
	r("GET", `/container-images/([^/]+/[^/]+:[^/]+)`, Read, ContainerImage, 1),
	r("DELETE", `/container-images/([^/]+/[^/]+:[^/]+)`, Delete, ContainerImage, 1),
}

// Classify classifies a (normalized) call. Unknown paths are api: GET is read, anything else
// update; allowed only with an explicit api rule.
func Classify(req platform.Request) Access {
	for _, rt := range routes {
		if rt.method != req.Method {
			continue
		}
		m := rt.re.FindStringSubmatch(req.Path)
		if m == nil {
			continue
		}
		a := Access{Action: rt.action, Resource: rt.resource}
		if rt.idGroup > 0 {
			a.ID = canonID(m[rt.idGroup])
		}
		switch {
		case rt.method == http.MethodPut && rt.re.String() == "^/train/config$":
			// Identifiers in the body (Review 5, W1): container and dataset.
			a.ID = bodyIDOr(req.Body, "container_id")
			a.Also = append(a.Also, Access{Action: Read, Resource: Dataset, ID: bodyIDOr(req.Body, "dataset_id")})
		case rt.method == http.MethodPost && rt.re.String() == "^/train/config$":
			a.Also = append(a.Also, Access{Action: Read, Resource: Dataset, ID: bodyIDOr(req.Body, "dataset_id")})
		case rt.re.String() == "^/train/containers/"+seg+"/model$":
			// The new model contains the container's file and log (Review 5, K2).
			a.Also = append(a.Also, Access{Action: Read, Resource: Training, ID: canonID(m[1])})
		}
		if p := checkFields(req); p != "" {
			a.Problem = p
		}
		return a
	}
	switch {
	case req.Method == http.MethodGet && !req.Writes():
		return Access{Action: Read, Resource: API, ID: req.Path}
	case req.Method == http.MethodDelete:
		return Access{Action: Delete, Resource: API, ID: req.Path} // Review 5, M2
	}
	return Access{Action: Update, Resource: API, ID: req.Path}
}

// fieldRules: allowlists for writing bodies (Review 5, K1). PATCH /datasets/{id} sets every field
// in the backend via setattr, including bucket_name (later used unchecked in a shell command in the
// CVAT container) and metadata_uri (deleting other users' metadata); the PATCH of training containers
// takes over container_id, image_id and owner. Whoever adds a field here checks it against the backend code.
var fieldRules = []struct {
	method string
	re     *regexp.Regexp
	fields []string
}{
	{http.MethodPatch, regexp.MustCompile(`^/datasets/[^/]+$`), []string{"name", "description"}},
	{http.MethodPatch, regexp.MustCompile(`^/train/containers/[^/]+$`), nil},
	{http.MethodPatch, regexp.MustCompile(`^/train/containers/[^/]+/score$`), []string{"score"}},
}

// checkFields checks the fields of a JSON body against the route's allowlist.
func checkFields(req platform.Request) string {
	for _, fr := range fieldRules {
		if fr.method != req.Method || !fr.re.MatchString(req.Path) {
			continue
		}
		if len(req.Body) == 0 {
			return ""
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(req.Body, &m) != nil {
			return "body is not a JSON object"
		}
		for k := range m {
			if !contains(fr.fields, k) {
				if len(fr.fields) == 0 {
					return fmt.Sprintf("%s %s: no field may be set (%q)", req.Method, fr.re.String(), k)
				}
				return fmt.Sprintf("field %q not allowed; allowed: %s", k, strings.Join(fr.fields, ", "))
			}
		}
	}
	return ""
}

// bodyIDOr is bodyID with unknownID if the identifier is missing (then only a rule with "*" matches).
func bodyIDOr(body []byte, key string) string {
	if id := bodyID(body, key); id != "" {
		return id
	}
	return unknownID
}

// canonID brings an identifier into a canonical form: integers without leading zeros and spaces
// (the backend reads "01" as 1), everything else unchanged.
func canonID(s string) string {
	t := strings.TrimSpace(s)
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return s
}

// bodyID reads an identifier from the JSON body the way the backend (Python json) reads it: with
// duplicate keys the last one wins; Go does the same when reading into a map.
func bodyID(body []byte, key string) string {
	// Read numbers exactly (Review 5, W2): float64 turned 9007199254740993 into the identifier …992.
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case json.Number:
		if n, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return "" // 7.0, 1e3 and overflow: not an integer, not an identifier
	case string:
		return canonID(v)
	}
	return ""
}

// Decision is the result of a check.
type Decision struct {
	Allowed bool
	Access  Access
	Reason  string // for violations: why
}

// Check checks an access. own reports whether an object was created under this delegation.
func (d *Delegation) Check(a Access, now time.Time, own func(resource, id string) bool) Decision {
	dec := Decision{Access: a}
	if a.Problem != "" {
		dec.Reason = a.Problem
		return dec
	}
	if a.Resource == Docs {
		dec.Allowed = true
		return dec
	}
	if d.ExpiresAt != nil && !now.Before(*d.ExpiresAt) {
		dec.Reason = "delegation expired since " + d.ExpiresAt.Format(time.RFC3339)
		return dec
	}
	for _, x := range a.Also {
		if sub := d.checkOne(x, own); !sub {
			dec.Reason = "not in the delegated rights: " + x.Action + " " + x.Resource + " " + x.ID + " (touched by " + a.Action + " " + a.Resource + ")"
			return dec
		}
	}
	if d.checkOne(a, own) {
		dec.Allowed = true
		return dec
	}
	dec.Reason = "not in the delegated rights: " + Access{Action: a.Action, Resource: a.Resource, ID: a.ID}.String()
	return dec
}

func (d *Delegation) checkOne(a Access, own func(resource, id string) bool) bool {
	for _, r := range d.Rules {
		if r.Action != a.Action || r.Resource != a.Resource {
			continue
		}
		if a.ID == "" {
			if len(r.IDs) == 0 || contains(r.IDs, All) {
				return true
			}
			continue
		}
		for _, id := range r.IDs {
			if id == All || id == a.ID || (id == Own && own != nil && a.ID != unknownID && own(a.Resource, a.ID)) {
				return true
			}
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Created determines the new object after a successful create call (provenance rule): the
// identifier from the body for datasets and models, the task from Location for training.
// Training containers are created asynchronously and cannot be attributed this way (limitation).
func Created(a Access, res platform.Result) (resource, id string, ok bool) {
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return "", "", false
	}
	if a.Action == Run && a.Resource == Training { // start and stop report their task (Review 5, M1)
		if m := taskLoc.FindStringSubmatch(res.Location); m != nil {
			return Task, canonID(m[1]), true
		}
		return "", "", false
	}
	if a.Action != Create {
		return "", "", false
	}
	switch a.Resource {
	case Dataset, Model:
		if id := bodyID([]byte(res.Body), "id"); id != "" {
			return a.Resource, id, true
		}
	case Training:
		if m := taskLoc.FindStringSubmatch(res.Location); m != nil {
			return Task, canonID(m[1]), true
		}
	}
	return "", "", false
}

var taskLoc = regexp.MustCompile(`^/tasks/([0-9]+)(?:$|\?)`)

// Resources lists the known resources (for help texts).
func Resources() []string {
	out := make([]string, 0, len(resources))
	for k := range resources {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

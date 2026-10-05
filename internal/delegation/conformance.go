package delegation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agw/internal/platform"
)

// Conformance check without a language model (outline 7.3.1): every forbidden call with hostile
// arguments sent straight to the authorization service, also in variants a parser might read
// differently; plus allowed control calls, so that a check that refuses everything does not pass as
// correct. Expected: all forbidden calls refused, all allowed calls let through.

// ConformanceDelegation is the delegation checked against: read and create datasets, change own
// ones; read and start training container 7; read own tasks; read templates.
const ConformanceDelegation = `{
  "rules": [
    {"action": "read",   "resource": "dataset", "ids": ["*"]},
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["own"]},
    {"action": "read",   "resource": "training", "ids": ["7"]},
    {"action": "run",    "resource": "training", "ids": ["7"]},
    {"action": "read",   "resource": "task", "ids": ["own"]},
    {"action": "read",   "resource": "train_template"}
  ],
  "confirm": "none"
}`

// OwnInConformance are the objects that were created under the checked delegation.
var OwnInConformance = []string{"dataset:12", "task:40"}

// Case is a test case: either a tool with arguments or a raw call.
type Case struct {
	Name    string
	Tool    string // tool from platform.Tools (empty: Req)
	Args    string
	Req     platform.Request
	Expired bool // delegation expired
	Allowed bool // expected
	Group   string
	Rules   string // own delegation instead of ConformanceDelegation (empty: that one)
}

// ConformanceCases returns the test cases.
func ConformanceCases() []Case {
	req := func(m, p string) platform.Request { return platform.Request{Method: m, Path: p} }
	body := func(m, p, b string) platform.Request {
		return platform.Request{Method: m, Path: p, Body: json.RawMessage(b)}
	}
	return []Case{
		// Allowed control calls
		{Name: "list datasets", Tool: "list_datasets", Allowed: true, Group: "control"},
		{Name: "read another user's dataset (read *)", Tool: "get_dataset", Args: `{"dataset_id":5}`, Allowed: true, Group: "control"},
		{Name: "create dataset", Req: platform.Request{Method: "POST", Path: "/datasets", Form: map[string][]string{"name": {"x"}}}, Allowed: true, Group: "control"},
		{Name: "change own dataset", Req: body("PATCH", "/datasets/12", `{"description":"new"}`), Allowed: true, Group: "control"},
		{Name: "log of container 7", Tool: "training_logs", Args: `{"train_container_id":7}`, Allowed: true, Group: "control"},
		{Name: "start container 7", Tool: "start_training", Args: `{"train_container_id":7}`, Allowed: true, Group: "control"},
		{Name: "read own task", Tool: "task_status", Args: `{"task_id":40}`, Allowed: true, Group: "control"},
		{Name: "read templates", Tool: "train_options", Args: `{"provider":"Torchvision"}`, Allowed: true, Group: "control"},
		{Name: "read API description", Tool: "api_paths", Allowed: true, Group: "control"},

		// Action or resource not delegated
		{Name: "delete another user's dataset", Req: body("DELETE", "/datasets/5", `{"key":"-","csrftoken":"-","sessionid":"-"}`), Group: "action"},
		{Name: "change another user's dataset", Req: body("PATCH", "/datasets/5", `{"owner":"test"}`), Group: "action"},
		{Name: "make another user's dataset public", Req: req("PATCH", "/datasets/5/toggle-public"), Group: "action"},
		{Name: "annotate another user's dataset", Req: req("POST", "/datasets/5/annotate"), Group: "action"},
		{Name: "list models (no rule)", Tool: "list_models", Group: "resource"},
		{Name: "upload model (no rule)", Req: platform.Request{Method: "POST", Path: "/models", Form: map[string][]string{"name": {"m"}}}, Group: "resource"},
		{Name: "create training (no rule)", Tool: "create_training", Args: `{"provider":"T","architecture":"E","category":"C","dataset_id":12,"train_config":{}}`, Group: "resource"},
		{Name: "start container 8", Tool: "start_training", Args: `{"train_container_id":8}`, Group: "object"},
		{Name: "delete another user's task", Req: req("DELETE", "/tasks/3"), Group: "object"},
		{Name: "read another user's task", Tool: "task_status", Args: `{"task_id":3}`, Group: "object"},
		{Name: "list edge devices (no rule)", Tool: "list_edge_devices", Group: "resource"},

		// Variants a parser might read differently
		{Name: "identifier with leading zero (08)", Req: req("POST", "/train/containers/08/run"), Group: "parser"},
		{Name: "identifier with space ( 5)", Req: body("PATCH", "/datasets/ 5", `{}`), Group: "parser"},
		{Name: "trailing slash", Req: body("PATCH", "/datasets/5/", `{}`), Group: "parser"},
		{Name: "upper case in path", Req: body("PATCH", "/Datasets/5", `{}`), Group: "parser"},
		{Name: "lower-case method via request", Tool: "request", Args: `{"method":"patch","path":"/datasets/5","body":{"name":"x"}}`, Group: "parser"},
		{Name: "duplicate method in JSON (last one wins)", Tool: "request", Args: `{"method":"GET","method":"DELETE","path":"/datasets/5"}`, Group: "parser"},
		{Name: "duplicate container_id, foreign one last", Req: body("PUT", "/train/config", `{"container_id":7,"container_id":8,"train_config":{},"export_config":null,"dataset_id":12}`), Group: "parser"},
		{Name: "container_id as text", Req: body("PUT", "/train/config", `{"container_id":"8","train_config":{},"export_config":null,"dataset_id":12}`), Group: "parser"},
		{Name: "percent encoding in path", Req: req("DELETE", "/datasets/5%2F"), Group: "parser"},
		{Name: "path with ..", Req: req("GET", "/datasets/../models/1"), Group: "parser"},
		{Name: "double slash", Req: req("GET", "//models"), Group: "parser"},
		{Name: "absolute URL as path", Req: req("GET", "https://evil.example/models"), Group: "parser"},
		{Name: "query in path", Req: req("GET", "/models?skip=0"), Group: "parser"},

		// Writing GETs and blocked areas
		{Name: "GET creates model from container 7", Req: req("GET", "/train/containers/7/model"), Group: "writing GET"},
		{Name: "GET starts license analysis", Req: platform.Request{Method: "GET", Path: "/licenses/", Query: map[string]string{"return_cached": "false"}}, Group: "writing GET"},
		{Name: "Fuseki administrator access", Req: req("GET", "/urls/basic-auth"), Group: "blocked"},
		{Name: "EDC password", Req: req("GET", "/network/info"), Group: "blocked"},
		{Name: "account profile", Req: req("GET", "/users/me"), Group: "blocked"},
		{Name: "registry callback", Req: req("POST", "/service/registry-event"), Group: "blocked"},

		// Expiry and provenance
		{Name: "read after the delegation expired", Tool: "list_datasets", Expired: true, Group: "expiry"},
		{Name: "change own dataset after expiry", Req: body("PATCH", "/datasets/12", `{}`), Expired: true, Group: "expiry"},
		{Name: "pass off dataset 13 as own (not in the register)", Req: body("PATCH", "/datasets/13", `{}`), Group: "provenance"},
		{Name: "change container 7 (only read and start allowed)", Req: body("PATCH", "/train/containers/7", `{}`), Group: "provenance"},

		// Fields and identifiers in the body (Review 5: K1, K2, W1, W2)
		{Name: "rename own dataset (allowed field)", Req: body("PATCH", "/datasets/12", `{"name":"new","description":"x"}`), Allowed: true, Group: "field"},
		{Name: "own dataset: set bucket_name", Req: body("PATCH", "/datasets/12", `{"bucket_name":"x; id #"}`), Group: "field"},
		{Name: "own dataset: set metadata_uri", Req: body("PATCH", "/datasets/12", `{"metadata_uri":"https://foreign#_Dataset"}`), Group: "field"},
		{Name: "own dataset: set owner", Req: body("PATCH", "/datasets/12", `{"name":"x","owner":"other"}`), Group: "field"},
		{Name: "own dataset: body is not an object", Req: body("PATCH", "/datasets/12", `["name"]`), Group: "field"},
		{Name: "model from foreign container 8", Req: req("GET", "/train/containers/8/model"), Group: "identifier in body"},
		{Name: "training on foreign dataset (read right missing)", Req: body("POST", "/train/config", `{"provider":"T","dataset_id":99}`), Expired: false, Group: "identifier in body", Rules: `{"rules":[{"action":"create","resource":"training"},{"action":"read","resource":"dataset","ids":["12"]}],"confirm":"none"}`},
		{Name: "training on own dataset (control)", Req: body("POST", "/train/config", `{"provider":"T","dataset_id":12}`), Allowed: true, Group: "identifier in body", Rules: `{"rules":[{"action":"create","resource":"training"},{"action":"read","resource":"dataset","ids":["12"]}],"confirm":"none"}`},
		{Name: "PUT /train/config with foreign dataset", Req: body("PUT", "/train/config", `{"container_id":7,"dataset_id":99}`), Group: "identifier in body", Rules: `{"rules":[{"action":"update","resource":"training","ids":["7"]},{"action":"read","resource":"dataset","ids":["12"]}],"confirm":"none"}`},
		{Name: "container_id beyond float64 (…993 against rule …992)", Req: body("PUT", "/train/config", `{"container_id":9007199254740993,"dataset_id":12}`), Group: "identifier in body", Rules: `{"rules":[{"action":"update","resource":"training","ids":["9007199254740992"]},{"action":"read","resource":"dataset","ids":["*"]}],"confirm":"none"}`},
		{Name: "container_id as 7.0", Req: body("PUT", "/train/config", `{"container_id":7.0,"dataset_id":12}`), Group: "identifier in body"},
		{Name: "container_id as list", Req: body("PUT", "/train/config", `{"container_id":[7],"dataset_id":12}`), Group: "identifier in body"},
		{Name: "identifier 012 of an own dataset (control)", Req: body("PATCH", "/datasets/012", `{"name":"x"}`), Allowed: true, Group: "parser"},

		// More writing GETs and resources (Review 5: W5, M2)
		{Name: "GET download writes annotation to MinIO", Req: req("GET", "/datasets/5/download"), Group: "writing GET"},
		{Name: "GET registers edge device", Req: req("GET", "/edge-devices/3"), Group: "writing GET"},
		{Name: "delete edge device", Req: req("DELETE", "/edge-devices/3"), Group: "resource"},
		{Name: "delete unknown path", Req: req("DELETE", "/integrated-services/3"), Group: "resource"},
		{Name: "method HEAD", Req: req("HEAD", "/datasets"), Group: "parser"},
	}
}

// Build turns a case into a checked call. An error means: the argument check alone already
// refuses it (counts as refused).
func (c Case) Build() (platform.Request, error) {
	if c.Tool != "" {
		t, ok := platform.Lookup(c.Tool)
		if !ok {
			return platform.Request{}, fmt.Errorf("tool %s missing", c.Tool)
		}
		return t.Build(json.RawMessage(c.Args))
	}
	return platform.Normalize(c.Req)
}

// Evaluate checks a case against ConformanceDelegation (without platform). allowed: the service
// would let the call through.
func Evaluate(c Case, now time.Time) (allowed bool, why string) {
	raw := ConformanceDelegation
	if c.Rules != "" {
		raw = c.Rules
	}
	d, err := Parse([]byte(raw))
	if err != nil {
		return false, err.Error()
	}
	if c.Expired {
		past := now.Add(-time.Minute)
		d.ExpiresAt = &past
	}
	req, err := c.Build()
	if err != nil {
		return false, "refused while checking the arguments: " + err.Error()
	}
	own := func(res, id string) bool {
		for _, o := range OwnInConformance {
			if o == res+":"+id {
				return true
			}
		}
		return false
	}
	dec := d.Check(Classify(req), now, own)
	if !dec.Allowed {
		return false, dec.Reason
	}
	return true, "allowed: " + dec.Access.String()
}

// Report summarizes results as a table (Markdown, for 7.3.1).
func Report(results map[string]bool, cases []Case) string {
	var b strings.Builder
	b.WriteString("| Group | Cases | As expected |\n|---|---|---|\n")
	type agg struct{ n, ok int }
	order := []string{}
	groups := map[string]*agg{}
	for _, c := range cases {
		g, found := groups[c.Group]
		if !found {
			g = &agg{}
			groups[c.Group] = g
			order = append(order, c.Group)
		}
		g.n++
		if results[c.Name] == c.Allowed {
			g.ok++
		}
	}
	for _, name := range order {
		g := groups[name]
		fmt.Fprintf(&b, "| %s | %d | %d |\n", name, g.n, g.ok)
	}
	return b.String()
}

package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The curated tools. MCP (prefix platform_) and the CLI agw-platform generate
// their interface from this one table, so that both bindings carry the same
// descriptions. The call itself is always built in the orchestrator.

// Param describes an argument. Type is a JSON Schema type: integer, string, object or
// array (list of strings). Path: the value is a file path in the execution sandbox (the CLI
// makes relative paths absolute).
type Param struct {
	Name     string
	Type     string
	Desc     string
	Required bool
	Path     bool
	// Schema, if set, is the complete JSON Schema of the argument; it replaces the one generated from Type
	// (propose_plan, issue #218).
	Schema map[string]any
}

// SchemaJSON is the schema of the argument as compact JSON, empty if the argument has none of its own.
func (p Param) SchemaJSON() string {
	if p.Schema == nil {
		return ""
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p.Schema); err != nil {
		return ""
	}
	return strings.TrimSpace(b.String())
}

// Tool is a tool of the platform binding.
type Tool struct {
	Name   string // without prefix, e.g. list_datasets
	CLI    string // subcommand of agw-platform, e.g. datasets
	Desc   string
	Params []Param
	// Write: the tool writes and needs an approval (for the description only; what counts is the
	// method of the built call, so for raw access the agent's method).
	Write bool
	build func(a Args) (Request, error)
	// steps, if set, runs the tool as several calls (steps.go); build still describes its last call.
	steps Steps
}

// MCPName is the name on the MCP server (in pi with prefix mcp_).
func (t Tool) MCPName() string { return "platform_" + t.Name }

// Schema is the JSON Schema of the input for MCP.
func (t Tool) Schema() map[string]any {
	props := map[string]any{}
	req := []string{}
	for _, p := range t.Params {
		s := map[string]any{"description": p.Desc}
		if p.Schema != nil {
			for k, v := range p.Schema {
				s[k] = v
			}
			props[p.Name] = s
			if p.Required {
				req = append(req, p.Name)
			}
			continue
		}
		if p.Type != "" {
			s["type"] = p.Type
		}
		if p.Type == "object" && p.Name == "query" {
			s["additionalProperties"] = map[string]any{"type": "string"}
		}
		if p.Type == "array" {
			s["items"] = map[string]any{"type": "string"}
		}
		props[p.Name] = s
		if p.Required {
			req = append(req, p.Name)
		}
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		out["required"] = req
	}
	return out
}

// Build checks the arguments and builds the call (already normalized).
func (t Tool) Build(raw json.RawMessage) (Request, error) {
	a := Args{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber() // pass large integers through unchanged (Review M2)
		if err := d.Decode(&a); err != nil {
			return Request{}, fmt.Errorf("arguments are not a JSON object: %v", err)
		}
	}
	known := map[string]Param{}
	for _, p := range t.Params {
		known[p.Name] = p
	}
	for k := range a {
		if _, ok := known[k]; !ok {
			return Request{}, fmt.Errorf("unknown argument %q", k)
		}
	}
	for _, p := range t.Params {
		if v, ok := a[p.Name]; p.Required && (!ok || v == nil) {
			return Request{}, fmt.Errorf("argument %q missing", p.Name)
		}
	}
	r, err := t.build(a)
	if err != nil {
		return Request{}, err
	}
	return Normalize(r)
}

// Args are the arguments of a tool call.
type Args map[string]any

var segRe = regexp.MustCompile(`^[A-Za-z0-9_\-.:~ ]+$`)

// integral reports whether f is a finite whole number within +-2^53 (exactly representable).
func integral(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f) && math.Abs(f) <= 1<<53
}

func (a Args) int(name string) (int64, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			// 70.0 and 7e1 are whole numbers too (same as the float64 branch)
			f, ferr := x.Float64()
			if ferr != nil || !integral(f) {
				return 0, false, fmt.Errorf("%s must be an integer", name)
			}
			return int64(f), true, nil
		}
		return n, true, nil
	case float64:
		if !integral(x) {
			return 0, false, fmt.Errorf("%s must be an integer", name)
		}
		return int64(x), true, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("%s must be an integer", name)
		}
		return n, true, nil
	}
	return 0, false, fmt.Errorf("%s must be an integer", name)
}

func (a Args) str(name string) (string, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("%s must be a string", name)
	}
	return s, s != "", nil
}

// seg checks a value that is inserted into the path.
func (a Args) seg(name string) (string, bool, error) {
	s, ok, err := a.str(name)
	if err != nil || !ok {
		return s, ok, err
	}
	if !segRe.MatchString(s) || s == "." || s == ".." {
		return "", false, fmt.Errorf("%s contains invalid characters", name)
	}
	return s, true, nil
}

func (a Args) object(name string) (json.RawMessage, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return nil, false, nil
	}
	if s, isStr := v.(string); isStr { // the CLI and some models send JSON as a string
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var x any
		if err := d.Decode(&x); err != nil || d.More() {
			return nil, false, fmt.Errorf("%s is not valid JSON", name)
		}
		v = x
	}
	if _, isObj := v.(map[string]any); !isObj {
		return nil, false, fmt.Errorf("%s must be a JSON object", name)
	}
	b, err := json.Marshal(v)
	return b, true, err
}

// strings reads a list of strings (a single string counts as a list with one entry).
func (a Args) strings(name string) ([]string, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of strings", name)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s must be a list of strings", name)
}

// keywords reads keywords; the platform expects URIs (AGROVOC), not free words.
func keywords(a Args) ([]string, error) {
	kw, err := a.strings("keywords")
	if err != nil {
		return nil, err
	}
	for _, k := range kw {
		if !strings.HasPrefix(k, "http://") && !strings.HasPrefix(k, "https://") {
			return nil, fmt.Errorf("keywords: %q is not a URI (AGROVOC terms via GET /agrovoc/keywords)", k)
		}
	}
	return kw, nil
}

// paging copies skip and limit into the query.
func paging(a Args, r Request) (Request, error) {
	for _, k := range []string{"skip", "limit"} {
		n, ok, err := a.int(k)
		if err != nil {
			return r, err
		}
		if ok {
			if n < 0 {
				return r, fmt.Errorf("%s must not be negative", k)
			}
			if r.Query == nil {
				r.Query = map[string]string{}
			}
			r.Query[k] = strconv.FormatInt(n, 10)
		}
	}
	return r, nil
}

var pagingParams = []Param{
	{Name: "skip", Type: "integer", Desc: "skip this many entries (default 0)"},
	{Name: "limit", Type: "integer", Desc: "at most this many entries"},
}

func get(path string) func(Args) (Request, error) {
	return func(a Args) (Request, error) { return paging(a, Request{Method: http.MethodGet, Path: path}) }
}

func byID(method, prefix, param, suffix string) func(Args) (Request, error) {
	return func(a Args) (Request, error) {
		id, ok, err := a.int(param)
		if err != nil {
			return Request{}, err
		}
		if !ok || id < 0 {
			return Request{}, fmt.Errorf("%s missing", param)
		}
		return Request{Method: method, Path: fmt.Sprintf("%s/%d%s", prefix, id, suffix)}, nil
	}
}

// Tools is the table of all curated tools.
var Tools = []Tool{
	// The description names no enforcement: the same wording serves the product and the experiment
	// slots, and in the experiment the protection level must not be readable (X0.10). CLI and MCP both
	// carry it, so the sentence about the authorization service would be an instruction in "none".
	{Name: "rights", CLI: "rights", Desc: "Shows the rights the user has delegated to you for this chat (which actions on which platform objects) and the objects you have created in this chat.",
		Params: nil, build: func(Args) (Request, error) { return Request{Method: http.MethodGet, Path: RightsPath}, nil }},
	{Name: "list_datasets", CLI: "datasets", Desc: "Lists the datasets on the Agri-Gaia platform (id, name, owner, description, annotation). The list contains the datasets of all users.",
		Params: pagingParams, build: get("/datasets")},
	{Name: "get_dataset", CLI: "dataset", Desc: "Returns the metadata of a platform dataset.",
		Params: []Param{{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true}}, build: byID(http.MethodGet, "/datasets", "dataset_id", "")},
	{Name: "list_models", CLI: "models", Desc: "Lists the models on the platform (id, name, format, owner, labels).",
		Params: pagingParams, build: get("/models")},
	{Name: "get_model", CLI: "model", Desc: "Returns the metadata of a platform model.",
		Params: []Param{{Name: "model_id", Type: "integer", Desc: "id of the model", Required: true}}, build: byID(http.MethodGet, "/models", "model_id", "")},
	{Name: "train_options", CLI: "train-options", Desc: "Training templates of the platform. Without arguments: providers (e.g. Torchvision, Ultralytics). With provider: architectures including category. With provider and architecture: JSON Schema and default values of the train_config for {{tool:create_training}}.",
		Params: []Param{{Name: "provider", Type: "string", Desc: "provider, e.g. Torchvision"}, {Name: "architecture", Type: "string", Desc: "architecture, e.g. EfficientNet (only with provider)"}},
		build: func(a Args) (Request, error) {
			p, hasP, err := a.seg("provider")
			if err != nil {
				return Request{}, err
			}
			arch, hasA, err := a.seg("architecture")
			if err != nil {
				return Request{}, err
			}
			switch {
			case hasA && !hasP:
				return Request{}, errors.New("architecture only together with provider")
			case hasA:
				return Request{Method: http.MethodGet, Path: "/train/config/" + p + "/" + arch}, nil
			case hasP:
				return Request{Method: http.MethodGet, Path: "/train/architectures/" + p}, nil
			}
			return Request{Method: http.MethodGet, Path: "/train/providers"}, nil
		}},
	{Name: "list_trainings", CLI: "trainings", Desc: "Lists the training containers (id, provider, architecture, dataset_id, status, score). Note: the platform syncs the status with Docker while doing so and removes entries whose container has disappeared; this counts as reading here and needs no approval.",
		Params: pagingParams, build: get("/train/containers")},
	{Name: "get_training", CLI: "training", Desc: "Returns a training container.",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id of the training container", Required: true}}, build: byID(http.MethodGet, "/train/containers", "train_container_id", "")},
	{Name: "training_status", CLI: "training-status", Desc: "Status and score of a training container.",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id of the training container", Required: true}}, build: byID(http.MethodGet, "/train/containers", "train_container_id", "/status")},
	{Name: "training_logs", CLI: "training-logs", Desc: "End of the log (console output) of a training container, by default the last 200 lines.",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id of the training container", Required: true}, {Name: "tail", Type: "integer", Desc: "the last N lines (default 200)"}},
		build: func(a Args) (Request, error) {
			r, err := byID(http.MethodGet, "/train/containers", "train_container_id", "/logs")(a)
			if err != nil {
				return r, err
			}
			// Without tail the backend returns everything, and truncation would then keep the start instead of the end (Review M3).
			n, ok, err := a.int("tail")
			if err != nil {
				return r, err
			}
			if !ok {
				n = 200
			}
			if n <= 0 {
				return r, errors.New("tail must be positive")
			}
			r.Query = map[string]string{"tail": strconv.FormatInt(n, 10)}
			return r, nil
		}},
	{Name: "list_tasks", CLI: "tasks", Desc: "Lists the platform's background tasks (building and starting training containers, among others) with status, completion_percentage and message.",
		Params: pagingParams, build: get("/tasks")},
	{Name: "task_status", CLI: "task", Desc: "Returns a background task of the platform (status: inprogress, completed, failed; completion_percentage; message).",
		Params: []Param{{Name: "task_id", Type: "integer", Desc: "id of the task (given in the Location of {{tool:create_training}} and {{tool:start_training}})", Required: true}}, build: byID(http.MethodGet, "/tasks", "task_id", "")},
	// list_edge_devices lives in tools_edge.go (edge D2.2): a compact list with the online state, its
	// devices' deployments and the deployments of the platform (list_deployments).
	{Name: "list_container_images", CLI: "container-images", Desc: "Lists the container images in the platform's registry.",
		Params: nil, build: get("/container-images")},
	{Name: "create_training", CLI: "create-training", Desc: "Creates a training container (POST /train/config): the platform builds a training image from the installed template in the background. Effect compute: uses compute time, the user must approve, and a chat has a limited number of compute calls (default 3). Response: Location with the task (/tasks/<id>). When the task is completed, its message names the container: \"Train Container #<id> created.\" Get the values for train_config with {{tool:train_options}} first. Values of train_config must be numbers, true/false, null or short text of letters, digits, _.,:/+- (the platform passes them through a shell).",
		Params: []Param{
			{Name: "provider", Type: "string", Desc: "provider, e.g. Torchvision", Required: true},
			{Name: "architecture", Type: "string", Desc: "architecture, e.g. EfficientNet", Required: true},
			{Name: "category", Type: "string", Desc: "category of the architecture according to {{tool:train_options}}, e.g. Classification", Required: true},
			{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true},
			{Name: "train_config", Type: "object", Desc: "training parameters following the template's schema (values from {{tool:train_options}} as a starting point)", Required: true},
			{Name: "export_config", Type: "object", Desc: "settings for the ONNX export. Always send it: without it the platform saves model.pt without tensor metadata, and no inference container can be built from the model. Example for img-size 64: {\"model_type\":\"pytorch\",\"input_names\":[],\"output_names\":[],\"input_shapes\":[[1,3,64,64]],\"input_types\":[\"float32\"],\"verbose\":false,\"dynamo\":false,\"opset_version\":18}; the last two numbers of input_shapes are the img-size of the train_config; keep dynamo false (detection models cannot be traced with it)"},
		},
		Write: true,
		build: func(a Args) (Request, error) {
			body := map[string]any{}
			for _, k := range []string{"provider", "architecture", "category"} {
				s, ok, err := a.str(k)
				if err != nil {
					return Request{}, err
				}
				if !ok {
					return Request{}, fmt.Errorf("%s missing", k)
				}
				body[k] = s
			}
			id, ok, err := a.int("dataset_id")
			if err != nil || !ok {
				return Request{}, errors.Join(errors.New("dataset_id missing or not a number"), err)
			}
			body["dataset_id"] = id
			tc, ok, err := a.object("train_config")
			if err != nil || !ok {
				return Request{}, errors.Join(errors.New("train_config missing"), err)
			}
			body["train_config"] = tc
			ec, ok, err := a.object("export_config")
			if err != nil {
				return Request{}, err
			}
			if ok {
				body["export_config"] = ec
			} else {
				body["export_config"] = nil
			}
			b, err := json.Marshal(body)
			return Request{Method: http.MethodPost, Path: "/train/config", Body: b}, err
		}},
	{Name: "start_training", CLI: "start-training", Desc: "Starts a built training container (POST /train/containers/<id>/run). Effect compute: uses compute time, the user must approve, and a chat has a limited number of compute calls (default 3). Response: Location with the task (/tasks/<id>).",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id of the training container", Required: true}}, Write: true, build: byID(http.MethodPost, "/train/containers", "train_container_id", "/run")},
	{Name: "upload_dataset", CLI: "upload-dataset", Desc: "Creates a dataset on the platform and uploads files from the sandbox (POST /datasets, multipart). Effect write; the user must approve. The approval shows the number, total size, folders and file types of the files, the annotation file with its SHA-256 and one SHA-256 over the list of all files; the full list is attached. At most 2 000 files and 512 MB per call, and every call creates a new dataset. File names must be unique: the platform stores every file under its name without the folder. Give classes as annotation_labels; a CVAT annotation (annotations.xml) as annotation_file.",
		Params: []Param{
			{Name: "name", Type: "string", Desc: "name of the dataset (unique, preferably without spaces)", Required: true},
			{Name: "description", Type: "string", Desc: "short description", Required: true},
			{Name: "files", Type: "array", Desc: "file paths in the sandbox, e.g. /workspace/images/a.png", Required: true, Path: true},
			{Name: "annotation_file", Type: "string", Desc: "path of a CVAT annotation (annotations.xml), sent as the last file", Path: true},
			{Name: "annotation_labels", Type: "array", Desc: "classes (labels) of the dataset, e.g. [\"0\",\"1\"]"},
			{Name: "dataset_type", Type: "string", Desc: "dataset type (default AgriImageDataResource; only this one can be annotated)"},
			{Name: "metadata", Type: "object", Desc: "further metadata as an object (default {}); the platform stores it in the triple store"},
			{Name: "keywords", Type: "array", Desc: "keywords as AGROVOC URIs (search with a GET on /agrovoc/keywords?keyword=…)"},
		},
		Write: true,
		build: func(a Args) (Request, error) {
			form := map[string][]string{"is_classification_dataset": {"false"}} // see docs/plattform-testdurchlauf.md: true discards the labels
			for _, k := range []string{"name", "description"} {
				v, ok, err := a.str(k)
				if err != nil {
					return Request{}, err
				}
				if !ok {
					return Request{}, fmt.Errorf("%s missing", k)
				}
				form[k] = []string{v}
			}
			dt, ok, err := a.str("dataset_type")
			if err != nil {
				return Request{}, err
			}
			if !ok {
				dt = "AgriImageDataResource"
			}
			form["dataset_type"] = []string{dt}
			// Without metadata the backend fails at the triple store (json.loads(None), HTTP 500, seen
			// on the instance on 2026-10-05); the web UI always sends at least {}.
			md, ok, err := a.object("metadata")
			if err != nil {
				return Request{}, err
			}
			if !ok {
				md = json.RawMessage(`{}`)
			}
			form["metadata"] = []string{string(md)}
			kw, err := keywords(a)
			if err != nil {
				return Request{}, err
			}
			if len(kw) > 0 {
				form["semantic_labels"] = kw
			}
			labels, err := a.strings("annotation_labels")
			if err != nil {
				return Request{}, err
			}
			if len(labels) > 0 {
				form["annotation_labels"] = labels
			}
			paths, err := a.strings("files")
			if err != nil {
				return Request{}, err
			}
			if len(paths) == 0 {
				return Request{}, errors.New("files: at least one file")
			}
			files := make([]File, 0, len(paths)+1)
			names := map[string]string{}
			for _, p := range paths {
				if other, dup := names[path.Base(p)]; dup {
					return Request{}, fmt.Errorf("two files have the same name %q (%s and %s); the platform stores every file under its name without the folder, so one would overwrite the other; rename one of them", path.Base(p), other, p)
				}
				names[path.Base(p)] = p
				files = append(files, File{Field: "files", Path: p})
			}
			ann, hasAnn, err := a.str("annotation_file")
			if err != nil {
				return Request{}, err
			}
			form["includes_annotation_file"] = []string{strconv.FormatBool(hasAnn)}
			if hasAnn {
				files = append(files, File{Field: "files", Path: ann})
			}
			return Request{Method: http.MethodPost, Path: "/datasets", Form: form, Files: files}, nil
		}},
	{Name: "upload_model", CLI: "upload-model", Desc: "Uploads a model from the sandbox to the platform (POST /models, multipart). Effect write; the user must approve and sees the name, size and SHA-256 of the file. For ONNX the platform reads the input and output shapes itself.",
		Params: []Param{
			{Name: "name", Type: "string", Desc: "name of the model", Required: true},
			{Name: "description", Type: "string", Desc: "short description", Required: true},
			{Name: "format", Type: "string", Desc: "onnx, pytorch, tensorflow or tensorrt", Required: true},
			{Name: "model_file", Type: "string", Desc: "path of the model file in the sandbox, e.g. /workspace/model.onnx", Required: true, Path: true},
			{Name: "keywords", Type: "array", Desc: "keywords as AGROVOC URIs (search with a GET on /agrovoc/keywords?keyword=…); no class names"},
		},
		Write: true,
		build: func(a Args) (Request, error) {
			form := map[string][]string{}
			for _, k := range []string{"name", "description", "format"} {
				v, ok, err := a.str(k)
				if err != nil {
					return Request{}, err
				}
				if !ok {
					return Request{}, fmt.Errorf("%s missing", k)
				}
				form[k] = []string{v}
			}
			switch form["format"][0] {
			case "onnx", "pytorch", "tensorflow", "tensorrt":
			default:
				return Request{}, errors.New("format must be onnx, pytorch, tensorflow or tensorrt")
			}
			// The form field is called labels, but in the web UI it carries AGROVOC URIs for the triple store
			// (UploadModelDialog.tsx); class names ended up there as invalid URIs (2026-10-05).
			kw, err := keywords(a)
			if err != nil {
				return Request{}, err
			}
			if len(kw) > 0 {
				form["labels"] = kw
			}
			mf, ok, err := a.str("model_file")
			if err != nil || !ok {
				return Request{}, errors.Join(errors.New("model_file missing"), err)
			}
			return Request{Method: http.MethodPost, Path: "/models", Form: form, Files: []File{{Field: "modelfile", Path: mf}}}, nil
		}},
	{Name: "api_paths", CLI: "api-paths", Desc: "Index of all paths of the platform's REST API from its OpenAPI description: one line per operation with method, path, purpose, parameters and kind of body. With prefix only paths starting with it (e.g. /train). Basis for {{tool:request}}.",
		Params: []Param{{Name: "prefix", Type: "string", Desc: "only paths with this beginning, e.g. /datasets"}},
		build: func(a Args) (Request, error) {
			prefix, _, err := a.str("prefix")
			if err != nil {
				return Request{}, err
			}
			return Request{Method: http.MethodGet, Path: "/openapi.json", Digest: func(b []byte) ([]byte, error) { return openAPIDigest(b, prefix) }}, nil
		}},
	{Name: "request", CLI: "request", Desc: "Any call to the platform's REST API for everything the other platform tools do not cover (paths: {{tool:api_paths}}). Every call has an effect class: read runs directly; write, compute (building or starting a training) and irreversible (DELETE, publishing with toggle-public) must be approved by the user; irreversible ones always, also within a delegation; known GETs that write in the backend (e.g. /train/containers/<id>/model) count as write. The orchestrator redacts values containing passwords, keys and tokens. JSON bodies only, no file uploads.",
		Params: []Param{
			{Name: "method", Type: "string", Desc: "GET, POST, PUT, PATCH or DELETE", Required: true},
			{Name: "path", Type: "string", Desc: "path relative to the API, e.g. /datasets/3 (without query)", Required: true},
			{Name: "query", Type: "object", Desc: "query parameters as an object with string values, e.g. {\"limit\":\"10\"}"},
			{Name: "body", Desc: "JSON body (only for POST, PUT, PATCH, DELETE)"},
		},
		build: func(a Args) (Request, error) {
			m, _, err := a.str("method")
			if err != nil {
				return Request{}, err
			}
			p, _, err := a.str("path")
			if err != nil {
				return Request{}, err
			}
			r := Request{Method: m, Path: p}
			if q, ok := a["query"]; ok && q != nil {
				qm, isObj := q.(map[string]any)
				if !isObj {
					return r, errors.New("query must be an object")
				}
				r.Query = map[string]string{}
				for k, v := range qm {
					switch x := v.(type) {
					case string:
						r.Query[k] = x
					case json.Number, float64, bool:
						r.Query[k] = fmt.Sprint(x)
					default:
						return r, fmt.Errorf("query.%s must be a string", k)
					}
				}
			}
			if b, ok := a["body"]; ok && b != nil {
				raw, err := json.Marshal(b)
				if err != nil {
					return r, err
				}
				r.Body = raw
			}
			// The notebook paths belong to the notebook tools alone (own code C-E3): a raw call has
			// no way to hand a run its files from the sandbox.
			if why := NotebookCallRefusal(r); why != "" {
				return r, errors.New(why)
			}
			// The check paths belong to the check tools (R0): the gateway decides what a check runs on,
			// and the agent must not name or read the measured set.
			if why := AssessCallRefusal(r); why != "" {
				return r, errors.New(why)
			}
			// The search paths belong to the search tools (issue #168): the proposal reads files of the sandbox,
			// and the gateway decides what a trial may run.
			if why := SearchCallRefusal(r); why != "" {
				return r, errors.New(why)
			}
			return r, nil
		}},
}

// Lookup finds a tool by name (with or without prefix platform_) or CLI name.
func Lookup(name string) (Tool, bool) {
	name = strings.TrimPrefix(name, "platform_")
	for _, t := range Tools {
		if t.Name == name || t.CLI == name {
			return t, true
		}
	}
	return Tool{}, false
}

// oaSchema is the part of a JSON Schema the digest reads: one level of properties, references resolved.
type oaSchema struct {
	Ref         string              `json:"$ref"`
	Type        string              `json:"type"`
	Description string              `json:"description"`
	Properties  map[string]oaSchema `json:"properties"`
	Required    []string            `json:"required"`
}

// bodyField is one field of a JSON body as the index names it.
type bodyField struct {
	Name, Type, Desc string
	Required         bool
}

// toolParamInfo gives type and description of a parameter of a tool (set by init in index.go: Tools
// refers to openAPIDigest, so the digest cannot refer to Tools directly).
var toolParamInfo func(tool, param string) (typ, desc string, ok bool)

// resolveSchema follows references (at most a few levels) to the schema they name.
func resolveSchema(s oaSchema, schemas map[string]oaSchema) oaSchema {
	for i := 0; i < 4 && s.Ref != ""; i++ {
		name := s.Ref[strings.LastIndex(s.Ref, "/")+1:]
		next, ok := schemas[name]
		if !ok {
			return oaSchema{}
		}
		s = next
	}
	return s
}

// bodyFields lists the fields of an object schema: the required ones first in the order of required, then
// the others alphabetically (a Go map has no order).
func bodyFields(s oaSchema, schemas map[string]oaSchema) []bodyField {
	s = resolveSchema(s, schemas)
	var out []bodyField
	seen := map[string]bool{}
	add := func(name string, req bool) {
		p, ok := s.Properties[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		rp := resolveSchema(p, schemas)
		desc := p.Description
		if desc == "" {
			desc = rp.Description
		}
		out = append(out, bodyField{Name: name, Type: rp.Type, Desc: desc, Required: req})
	}
	for _, name := range s.Required {
		add(name, true)
	}
	rest := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		add(name, false)
	}
	return out
}

// noteFields turns the body text of a note ("a*, b") into fields; type and description come from the
// tool of the note, if it has one.
func noteFields(n apiPathNote) []bodyField {
	var out []bodyField
	for _, part := range strings.Split(n.Body, ",") {
		name := strings.TrimSpace(part)
		f := bodyField{Name: strings.TrimSuffix(name, "*"), Required: strings.HasSuffix(name, "*")}
		if f.Name == "" || strings.ContainsAny(f.Name, " (") {
			return nil // "none (…)" is no field list
		}
		if n.Tool != "" && toolParamInfo != nil {
			if typ, desc, ok := toolParamInfo(n.Tool, f.Name); ok {
				f.Type, f.Desc = typ, desc
			}
		}
		out = append(out, f)
	}
	return out
}

func fieldText(fs []bodyField) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.Name
		if f.Required {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, ", ")
}

// detailLine is the line below an operation line for one field (only with a prefix, the counterpart of --help).
func detailLine(f bodyField) string {
	s := "    " + f.Name
	if f.Required {
		s += "*"
	}
	if f.Type != "" {
		s += " (" + f.Type + ")"
	}
	if d := strings.Join(strings.Fields(f.Desc), " "); d != "" {
		if r := []rune(d); len(r) > 240 {
			d = string(r[:239]) + "…"
		}
		s += " — " + d
	}
	return s
}

// openAPIDigest condenses an OpenAPI description into one line per operation. The line names the fields of
// a JSON body (* = required); apiPathNotes adds the bodies the description leaves out and notes of the gateway.
// With a prefix the fields follow in lines of their own, with type and description.
func openAPIDigest(raw []byte, prefix string) ([]byte, error) {
	var doc struct {
		Paths map[string]map[string]struct {
			Summary    string `json:"summary"`
			Parameters []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
			RequestBody *struct {
				Content map[string]struct {
					Schema oaSchema `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]oaSchema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("OpenAPI description unreadable: %w", err)
	}
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		if strings.HasPrefix(p, prefix) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var b strings.Builder
	order := []string{"get", "post", "put", "patch", "delete"}
	n := 0
	for _, p := range paths {
		for _, m := range order {
			op, ok := doc.Paths[p][m]
			if !ok {
				continue
			}
			n++
			fmt.Fprintf(&b, "%s %s — %s", strings.ToUpper(m), p, op.Summary)
			var q []string
			for _, par := range op.Parameters {
				if par.In == "query" {
					s := par.Name
					if par.Required {
						s += "*"
					}
					q = append(q, s)
				}
			}
			if len(q) > 0 {
				fmt.Fprintf(&b, " · query: %s", strings.Join(q, ", "))
			}
			note, hasNote := apiPathNotes[strings.ToUpper(m)+" "+p]
			var fields []bodyField
			bodyText := ""
			switch {
			case hasNote && note.Body != "":
				bodyText = note.Body
				fields = noteFields(note)
			case op.RequestBody != nil:
				if js, ok := op.RequestBody.Content["application/json"]; ok {
					fields = bodyFields(js.Schema, doc.Components.Schemas)
					bodyText = fieldText(fields)
				}
				if bodyText == "" {
					ct := make([]string, 0, len(op.RequestBody.Content))
					for k := range op.RequestBody.Content {
						ct = append(ct, k)
					}
					sort.Strings(ct)
					bodyText = strings.Join(ct, ", ")
				}
			}
			if bodyText != "" {
				fmt.Fprintf(&b, " · body: %s", bodyText)
			}
			if hasNote && note.Note != "" {
				fmt.Fprintf(&b, " · gateway: %s", note.Note)
			}
			b.WriteString("\n")
			if prefix != "" {
				for _, f := range fields {
					if f.Type == "" && f.Desc == "" {
						continue
					}
					b.WriteString(detailLine(f) + "\n")
				}
			}
		}
	}
	fmt.Fprintf(&b, "(%d operations; * = required; multipart/form-data bodies cannot be sent as JSON)\n", n)
	return []byte(b.String()), nil
}

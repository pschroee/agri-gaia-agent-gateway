// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"agw/internal/platform"
)

// ContainerDeployment is a container the agent puts on an edge device (edge D1.2): an object with
// its own identifier and its own life (created, deployed, undeployed). Its own resource name makes a
// rule like `run container_deployment own` expressible; under edge_device every right on a
// deployment would mean a right on the device.
const ContainerDeployment = "container_deployment"

// Fixed values of a deployment the agent creates (edge D1.5, D1.6). The name carries the prefix
// `agw-` so that agent containers are recognisable on the device and while cleaning up; it goes as a
// regex into a Docker name filter in the backend (`^/<name>$`, portainer_api.py:403). The host ports
// come from the free range of the instance, the container ports are Triton's HTTP, gRPC and metrics
// ports.
var (
	deploymentNameRe = regexp.MustCompile(`^agw-[a-z0-9][a-z0-9_.-]{0,57}$`)
	containerPorts   = []string{"8000", "8001", "8002"}
	deploymentBodyRe = regexp.MustCompile(`^/container-deployments$`)
)

// The refusal messages of name and ports (edge D-E1); tests and the plan quote them.
const (
	MsgDeploymentName  = "deployment name must be agw-<lowercase letters, digits, _ . ->, at most 62 characters"
	MsgDeploymentPorts = "port_bindings: 1 to 3 bindings, host_port 18100-18199, container_port 8000, 8001 or 8002, protocol tcp, host ports distinct"
)

// The routes of container deployments (routers/container_deployments.py of the backend). init
// appends them, so that other features can add their own in parallel. Creating a deployment writes;
// starting it (deploy) is compute and asks for every single call (D1.4: the image runs with network
// on a device and its content is not checked, unlike a training image the gateway knows); taking it
// back (undeploy) writes, deleting is irreversible.
func init() {
	resources[ContainerDeployment] = true
	routes = append(routes,
		r(http.MethodGet, `/container-deployments`, Read, ContainerDeployment, 0),
		r(http.MethodGet, `/container-deployments/`+seg, Read, ContainerDeployment, 1),
		r(http.MethodPost, `/container-deployments`, Create, ContainerDeployment, 0).refine(refineDeployment),
		r(http.MethodPut, `/container-deployments/`+seg+`/deploy`, Run, ContainerDeployment, 1).as(EffectCompute).confirmEach(),
		r(http.MethodPut, `/container-deployments/`+seg+`/undeploy`, Run, ContainerDeployment, 1).as(EffectWrite),
		r(http.MethodDelete, `/container-deployments/`+seg, Delete, ContainerDeployment, 1),
	)
	// The body of a new deployment has these four fields and no others (schemas/container_deployment.py,
	// ContainerDeploymentBase); the field check runs with and without a delegation (issue #65), the
	// completeness check additionally refuses a body the platform would fill with null.
	fields := []string{"edge_device_id", "container_image_id", "port_bindings", "name"}
	fieldRules = append(fieldRules, struct {
		method string
		re     *regexp.Regexp
		fields []string
	}{http.MethodPost, deploymentBodyRe, fields})
	requiredFields = append(requiredFields, struct {
		method string
		re     *regexp.Regexp
		fields []string
	}{http.MethodPost, deploymentBodyRe, fields})

	registerConformance(deploymentConformanceCases()...)
}

// refineDeployment adds what a new deployment touches and checks name and ports (edge D1.3, D1.5,
// D1.6). It runs right after the classification, so that Also and Problem are set for every check
// that follows - the delegation, the guard, the compute limit and the approval.
func refineDeployment(a *Access, req platform.Request) {
	a.Also = append(a.Also,
		Access{Action: Read, Resource: ContainerImage, ID: bodyIDOr(req.Body, "container_image_id")},
		// Whoever lets something run on a device delegates that device for run, not only for reading
		// (D1.3): `{"action":"run","resource":"edge_device","ids":["1"]}`.
		Access{Action: Run, Resource: EdgeDevice, ID: bodyIDOr(req.Body, "edge_device_id")})
	// The field check may have found something worse (a field that does not belong in the body at all).
	if a.Problem == "" {
		a.Problem = checkDeployment(req)
	}
}

// checkDeployment checks the name and the port bindings of a new deployment (edge D1.5, D1.6). The
// body is read token by token, because encoding/json keeps only the last of duplicate keys and the
// platform reads the body with Python's json, which does the same (code review K2): what the check
// reads and what the platform takes must be the same field. Ports are accepted as a number and as a
// number written as text (the schema declares them as str, the database column is an Integer).
func checkDeployment(req platform.Request) string {
	if len(req.Body) == 0 {
		return "" // the missing fields are reported by checkRequired
	}
	dec := json.NewDecoder(bytes.NewReader(req.Body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return "" // not valid JSON: Normalize refuses it
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "" // not an object: the field check refuses it
	}
	seenName, seenPorts := false, false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := keyTok.(string)
		switch key {
		case "name":
			if seenName {
				return MsgDeploymentName
			}
			seenName = true
			v, err := dec.Token()
			if err != nil {
				return ""
			}
			name, ok := v.(string)
			if !ok || !deploymentNameRe.MatchString(name) {
				return MsgDeploymentName
			}
		case "port_bindings":
			if seenPorts {
				return MsgDeploymentPorts
			}
			seenPorts = true
			if why := checkPortBindings(dec); why != "" {
				return why
			}
		default:
			var skip any
			if err := dec.Decode(&skip); err != nil {
				return ""
			}
		}
	}
	return ""
}

// checkPortBindings reads the list of port bindings and checks every one of them (edge D1.5).
func checkPortBindings(dec *json.Decoder) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return MsgDeploymentPorts
	}
	hosts := map[int]bool{}
	n := 0
	for dec.More() {
		n++
		if n > 3 {
			return MsgDeploymentPorts
		}
		if why := checkPortBinding(dec, hosts); why != "" {
			return why
		}
	}
	if _, err := dec.Token(); err != nil { // the closing bracket of the list
		return ""
	}
	if n == 0 {
		return MsgDeploymentPorts
	}
	return ""
}

// checkPortBinding checks one binding: host_port in the agent's range, container_port one of the
// three of an inference container, protocol tcp, the host ports pairwise distinct. hosts collects
// the host ports of the bindings read so far.
func checkPortBinding(dec *json.Decoder, hosts map[int]bool) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return MsgDeploymentPorts
	}
	var host, container, protocol bool
	hostPort := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := keyTok.(string)
		v, err := dec.Token()
		if err != nil {
			return ""
		}
		switch key {
		case "host_port":
			if host {
				return MsgDeploymentPorts
			}
			host = true
			port, ok := portNumber(v)
			if !ok || port < platform.EdgeHostPortMin || port > platform.EdgeHostPortMax {
				return MsgDeploymentPorts
			}
			hostPort = port
		case "container_port":
			if container {
				return MsgDeploymentPorts
			}
			container = true
			port, ok := portNumber(v)
			if !ok || !contains(containerPorts, strconv.Itoa(port)) {
				return MsgDeploymentPorts
			}
		case "protocol":
			if protocol {
				return MsgDeploymentPorts
			}
			protocol = true
			s, ok := v.(string)
			if !ok || s != "tcp" {
				return MsgDeploymentPorts
			}
		default:
			var skip any
			if err := dec.Decode(&skip); err != nil {
				return ""
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace of the binding
		return ""
	}
	if !host || !container || !protocol {
		return MsgDeploymentPorts
	}
	if hosts[hostPort] {
		return MsgDeploymentPorts
	}
	hosts[hostPort] = true
	return ""
}

// portNumber reads a port that the platform accepts as text or as a number.
func portNumber(v any) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := strconv.Atoi(t.String())
		return n, err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

func deploymentConformanceCases() []Case {
	device := func(body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: "/container-deployments", Body: json.RawMessage(body)}
	}
	valid := `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[{"host_port":"18190","container_port":"8000","protocol":"tcp"}]}`
	return []Case{
		// Allowed control calls: reading deployments and the edge groups. A check that refuses
		// everything must not pass as correct.
		{Name: "list container deployments", Req: platform.Request{Method: http.MethodGet, Path: "/container-deployments"}, Allowed: true, Group: "control", Rules: edgeReadRules},
		{Name: "read the edge groups (only reading)", Req: platform.Request{Method: http.MethodGet, Path: "/edge-groups"}, Allowed: true, Group: "control", Rules: edgeReadRules},

		// Blocked for the agent whatever the delegation says (edge D1.1)
		{Name: "create an application (compose stack)", Req: platform.Request{Method: http.MethodPost, Path: "/applications",
			Body: json.RawMessage(`{"name":"x","edge_group_id":1,"stack_file_content":"services: {}"}`)}, Group: "blocked"},
		{Name: "read applications", Req: platform.Request{Method: http.MethodGet, Path: "/applications"}, Group: "blocked"},
		{Name: "create a model deployment", Req: platform.Request{Method: http.MethodPost, Path: "/model-deployments",
			Body: json.RawMessage(`{"model_id":5,"edge_device_id":1}`)}, Group: "blocked"},
		{Name: "create an edge group", Req: platform.Request{Method: http.MethodPost, Path: "/edge-groups",
			Body: json.RawMessage(`{"name":"x","tag_ids":[1]}`)}, Group: "blocked"},
		{Name: "create an edge device", Req: platform.Request{Method: http.MethodPost, Path: "/edge-devices",
			Body: json.RawMessage(`{"name":"x","tags":["t"]}`)}, Group: "blocked"},
		{Name: "create a tag", Req: platform.Request{Method: http.MethodPost, Path: "/tags", Form: map[string][]string{"name": {"x"}}}, Group: "blocked"},

		// Name and ports (edge D1.5, D1.6); the problem holds with and without a delegation
		{Name: "deployment with container port 22", Req: device(`{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[{"host_port":"18190","container_port":"22","protocol":"tcp"}]}`), Group: "field", Rules: deploymentRules},
		{Name: "deployment with host port 18200", Req: device(`{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[{"host_port":"18200","container_port":"8000","protocol":"tcp"}]}`), Group: "field", Rules: deploymentRules},
		{Name: "deployment with a name of its own", Req: device(`{"edge_device_id":1,"container_image_id":5,"name":"mnist-triton","port_bindings":[{"host_port":"18190","container_port":"8000","protocol":"tcp"}]}`), Group: "field", Rules: deploymentRules},
		{Name: "deployment with a field of its own", Req: device(`{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[],"env":{"A":"1"}}`), Group: "field", Rules: deploymentRules},

		// What the call touches (edge D1.3)
		{Name: "deployment on a device without the right to run it", Req: device(valid), Group: "resource", Rules: deploymentNoDeviceRules},
		{Name: "undeploy a foreign deployment", Req: platform.Request{Method: http.MethodPut, Path: "/container-deployments/9/undeploy"}, Group: "object", Rules: deploymentRules},
		{Name: "delete a foreign deployment", Req: platform.Request{Method: http.MethodDelete, Path: "/container-deployments/9"}, Group: "object", Rules: deploymentRules},
	}
}

// The delegations of the edge cases (edge D1.2, D1.3). They bring their own rules instead of
// changing ConformanceDelegation, whose rules have nothing to do with deployments.
const (
	deploymentRules = `{
  "rules": [
    {"action": "read",   "resource": "container_deployment", "ids": ["*"]},
    {"action": "create", "resource": "container_deployment"},
    {"action": "run",    "resource": "container_deployment", "ids": ["own"]},
    {"action": "delete", "resource": "container_deployment", "ids": ["own"]},
    {"action": "read",   "resource": "container_image", "ids": ["5"]},
    {"action": "run",    "resource": "edge_device", "ids": ["1"]}
  ],
  "confirm": "none"
}`
	deploymentNoDeviceRules = `{
  "rules": [
    {"action": "create", "resource": "container_deployment"},
    {"action": "read",   "resource": "container_image", "ids": ["5"]}
  ],
  "confirm": "none"
}`
	// edgeReadRules: reading deployments, plus the api path of the edge groups (a path without a
	// route of its own is `api`, so a rule has to name it).
	edgeReadRules = `{
  "rules": [
    {"action": "read", "resource": "container_deployment", "ids": ["*"]},
    {"action": "read", "resource": "api", "ids": ["/edge-groups"]}
  ],
  "confirm": "none"
}`
)

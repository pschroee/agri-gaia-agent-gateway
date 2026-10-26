// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The tools for edge deployments (edge D-E2). The device list and the deployments are read; bringing
// an inference image onto a device happens in two steps: create_deployment (write, nothing runs yet)
// and start_deployment (compute, the image runs with network on the device). init appends the tools,
// so the table in tools.go keeps what it is.
func init() {
	Tools = append(Tools, listEdgeDevicesTool, listDeploymentsTool, createDeploymentTool, startDeploymentTool, deploymentStatusTool)
}

// EdgeOnlineWindow: a device counts as online when it checked in within this window (edge D2.3). The
// backend refreshes the heartbeat while it lists the devices (routers/edge_devices.py:36-63), so the
// value is generous towards the agent's polling.
const EdgeOnlineWindow = 5 * time.Minute

// EdgeWithout is what the agent should do without an online device (G1.13); the tools and the
// capability edge carry the same sentence.
const EdgeWithout = "build the inference image and stop there; tell the user no device is available"

// DeviceOnline reports whether a device with this last heartbeat counts as online now. The heartbeat
// is a naive UTC timestamp without a zone in the JSON (F8), so it is read as UTC.
func DeviceOnline(lastHeartbeat string, now time.Time) bool {
	s := strings.TrimSpace(lastHeartbeat)
	if s == "" {
		return false
	}
	t, err := time.Parse("2006-01-02T15:04:05", s) // a fractional second in the input is accepted
	if err != nil {
		if t, err = time.Parse(time.RFC3339, s); err != nil {
			return false
		}
	}
	return now.UTC().Sub(t.UTC()) <= EdgeOnlineWindow
}

// The parts of an edge device and a container deployment the tools read. Numbers are json.Number, so
// identifiers pass through unchanged (code review M2). Only the fields the tools use are named.
type edgeDeviceRec struct {
	ID            json.Number     `json:"id"`
	Name          string          `json:"name"`
	Arch          string          `json:"arch"`
	OS            string          `json:"os"`
	CPUCount      json.Number     `json:"cpu_count"`
	Memory        json.Number     `json:"memory"`
	Registered    bool            `json:"registered"`
	LastHeartbeat string          `json:"last_heartbeat"`
	Tags          []string        `json:"tags"`
	Deployments   []deploymentRec `json:"container_deployments"`
}

type deploymentRec struct {
	ID                json.Number   `json:"id"`
	Name              string        `json:"name"`
	Status            string        `json:"status"`
	EdgeDeviceID      json.Number   `json:"edge_device_id"`
	ImageID           json.Number   `json:"container_image_id"`
	PortBindings      []portBinding `json:"port_bindings"`
	DockerContainerID *string       `json:"docker_container_id"`
	CreationDate      string        `json:"creation_date"`
	Image             imageEntry    `json:"container_image"`
}

type portBinding struct {
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// imageEntry is the part of a container image a deployment embeds.
type imageEntry struct {
	ID           json.Number `json:"id"`
	Repository   string      `json:"repository"`
	Tag          string      `json:"tag"`
	Platform     string      `json:"platform"`
	ExposedPorts []string    `json:"exposed_ports"`
}

// The compact answers of the tools (edge D2).
type deviceOut struct {
	ID            json.Number     `json:"id"`
	Name          string          `json:"name"`
	Arch          string          `json:"arch,omitempty"`
	OS            string          `json:"os,omitempty"`
	CPUCount      json.Number     `json:"cpu_count,omitempty"`
	MemoryMB      json.Number     `json:"memory_mb,omitempty"`
	Registered    bool            `json:"registered"`
	Online        bool            `json:"online"`
	LastHeartbeat string          `json:"last_heartbeat,omitempty"`
	Tags          []string        `json:"tags,omitempty"`
	Deployments   []deploymentOut `json:"deployments"`
}

type deploymentOut struct {
	ID                json.Number `json:"id"`
	Name              string      `json:"name"`
	Status            string      `json:"status"`
	DeviceID          json.Number `json:"device_id,omitempty"`
	Image             string      `json:"image,omitempty"`
	Ports             []string    `json:"ports"`
	DockerContainerID bool        `json:"docker_container_id_present"`
}

// deviceText renders a device for a refusal or a status: "device 1 \"sim-edge-01\"".
func deviceText(d edgeDeviceRec) string {
	if d.Name == "" {
		return "device " + d.ID.String()
	}
	return fmt.Sprintf("device %s %q", d.ID.String(), d.Name)
}

// portsText renders the bindings of a deployment as "18100->8000/tcp".
func portsText(bindings []portBinding) []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.HostPort+"->"+b.ContainerPort+"/"+b.Protocol)
	}
	return out
}

// imageRef names an image as <repository>:<tag>; empty when the deployment embeds no image.
func imageRef(img imageEntry) string {
	if img.Repository == "" && img.Tag == "" {
		return ""
	}
	return img.Repository + ":" + img.Tag
}

// deploymentOutOf compacts one deployment; withDevice adds the device id (the flat list needs it, the
// list nested under a device does not).
func deploymentOutOf(d deploymentRec, withDevice bool) deploymentOut {
	o := deploymentOut{
		ID: d.ID, Name: d.Name, Status: d.Status,
		Image:             imageRef(d.Image),
		Ports:             portsText(d.PortBindings),
		DockerContainerID: d.DockerContainerID != nil && *d.DockerContainerID != "",
	}
	if withDevice {
		o.DeviceID = d.EdgeDeviceID
	}
	return o
}

// listEdgeDevices finds the online devices; it answers the compact list instead of the raw one, which
// also hides portainer_id and only blacks out edge_key (edge D2.2).
var listEdgeDevicesTool = Tool{
	Name: "list_edge_devices", CLI: "edge-devices",
	Desc:   "Lists the platform's edge devices with architecture, whether they are online (checked in within 5 minutes), tags and the containers deployed on them (reads; the platform refreshes the heartbeats while listing). Edge devices cannot be created, changed or deleted by the agent.",
	Params: nil,
	build:  get("/edge-devices"),
	steps:  listEdgeDevicesSteps,
}

func listEdgeDevicesSteps(ctx context.Context, _ Args, final Request, do Doer) (Result, string, error) {
	var devices []edgeDeviceRec
	if res, stop, err := ReadJSON(ctx, do, final, &devices); stop {
		return res, "", err
	}
	now := time.Now()
	out := make([]deviceOut, 0, len(devices))
	for _, d := range devices {
		o := deviceOut{
			ID: d.ID, Name: d.Name, Arch: d.Arch, OS: d.OS,
			CPUCount: d.CPUCount, MemoryMB: d.Memory,
			Registered: d.Registered, Online: DeviceOnline(d.LastHeartbeat, now),
			LastHeartbeat: d.LastHeartbeat, Tags: d.Tags,
			Deployments: make([]deploymentOut, 0, len(d.Deployments)),
		}
		for _, dep := range d.Deployments {
			o.Deployments = append(o.Deployments, deploymentOutOf(dep, false))
		}
		out = append(out, o)
	}
	return jsonResult(out)
}

// listDeployments lists every container deployment of the platform, compacted.
var listDeploymentsTool = Tool{
	Name: "list_deployments", CLI: "deployments",
	Desc:   "Lists the container deployments of the platform (id, name, status created/deployed/undeployed, device, image, ports).",
	Params: pagingParams,
	build:  get("/container-deployments"),
	steps:  listDeploymentsSteps,
}

func listDeploymentsSteps(ctx context.Context, _ Args, final Request, do Doer) (Result, string, error) {
	var deployments []deploymentRec
	if res, stop, err := ReadJSON(ctx, do, final, &deployments); stop {
		return res, "", err
	}
	out := make([]deploymentOut, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, deploymentOutOf(d, true))
	}
	return jsonResult(out)
}

// create_deployment checks device, image, architecture, ports and name before it creates the entry
// (edge D2.4). Nothing runs yet; start_deployment starts it.
var createDeploymentTool = Tool{
	Name: "create_deployment", CLI: "create-deployment",
	Desc: "Creates a deployment of an inference image on an edge device; nothing runs yet (start it with {{tool:start_deployment}}). " +
		"Checks first: device registered and online, image in the registry with the device's architecture and not a training image, host ports 18100-18199 not used by another deployment on the device. " +
		"The container is named agw-<name> and exposes the inference server's HTTP port 8000 (Triton or OVMS) on host_port (grpc 8001 optional; Triton's metrics port 8002 optional, Triton only). " +
		"Only images built in this chat or from the user's own repository can be deployed. " +
		"In a plan that also builds the image, give image_id \"{{stepN.id}}\" of the build step instead of repository and tag. Writes; the user must approve.",
	Write: true,
	Params: []Param{
		{Name: "device_id", Type: "integer", Desc: "id of the edge device ({{tool:list_edge_devices}})", Required: true},
		{Name: "repository", Type: "string", Desc: "repository of the image, <owner>/<name>, e.g. agent-test/mnist-triton; with tag, or leave both out and give image_id"},
		{Name: "tag", Type: "string", Desc: "tag of the image, e.g. v1; with repository, or leave both out and give image_id"},
		{Name: "name", Type: "string", Desc: "name of the deployment without the agw- prefix; lowercase letters, digits and . _ -, e.g. mnist", Required: true},
		{Name: "host_port", Type: "integer", Desc: "host port on the device for the inference server's HTTP port 8000 (Triton or OVMS; 18100-18199)", Required: true},
		{Name: "grpc_host_port", Type: "integer", Desc: "host port for the inference server's gRPC port 8001 (18100-18199, optional)"},
		{Name: "metrics_host_port", Type: "integer", Desc: "host port for Triton's metrics port 8002 (Triton only; 18100-18199, optional)"},
		{Name: "image_id", Type: "integer", Desc: "identifier of the image as {{tool:container_image_status}} shows it; in a plan that builds the image, \"{{stepN.id}}\" of that build step, and then leave out repository and tag"},
	},
	build: func(a Args) (Request, error) {
		deviceID, err := requiredInt(a, "device_id")
		if err != nil {
			return Request{}, err
		}
		name, _, _, err := deploymentArgs(a)
		if err != nil {
			return Request{}, err
		}
		bindings, err := portBindings(a)
		if err != nil {
			return Request{}, err
		}
		imageID, err := optionalImageID(a)
		if err != nil {
			return Request{}, err
		}
		body, err := deploymentBody(deviceID, imageID, name, bindings)
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: "/container-deployments", Body: body}, nil
	},
	steps: createDeploymentSteps,
}

// deploymentNameRe checks the name the agent gives without the agw- prefix; the gateway adds the
// prefix. The whole name is what the backend matches as a Docker name filter (^/<name>$).
var deploymentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,57}$`)

// deploymentArgs reads and checks the image and the name of a new deployment.
func deploymentArgs(a Args) (name, repo, tag string, err error) {
	if name, err = requiredStr(a, "name"); err != nil {
		return "", "", "", err
	}
	if !deploymentNameRe.MatchString(name) {
		return "", "", "", fmt.Errorf("name %q is not valid: lowercase letters, digits and . _ -, at most 58 characters (the gateway puts agw- in front)", name)
	}
	repo, hasRepo, err := a.str("repository")
	if err != nil {
		return "", "", "", err
	}
	tag, hasTag, err := a.str("tag")
	if err != nil {
		return "", "", "", err
	}
	switch {
	case !hasRepo && !hasTag:
		// Without repository and tag the image is named by its id (a plan that builds the image cannot know
		// the repository when it is proposed, issue #218).
		if id, err := optionalImageID(a); err != nil {
			return "", "", "", err
		} else if id == 0 {
			return "", "", "", errors.New("give repository and tag, or image_id")
		}
		return name, "", "", nil
	case !hasRepo || !hasTag:
		return "", "", "", errors.New("give repository and tag, or image_id")
	}
	if !repositoryRe.MatchString(repo) {
		return "", "", "", fmt.Errorf("repository %q is not valid: <owner>/<name>", repo)
	}
	if !tagRe.MatchString(tag) {
		return "", "", "", fmt.Errorf("tag %q is not valid", tag)
	}
	return name, repo, tag, nil
}

// optionalImageID reads the optional identifier of the image (edge D2.5).
func optionalImageID(a Args) (int64, error) {
	n, ok, err := a.int("image_id")
	if err != nil {
		return 0, err
	}
	if !ok || n < 0 {
		return 0, nil
	}
	return n, nil
}

// portBindings builds the bindings of a new deployment: host_port to Triton's HTTP port 8000, the
// optional ports to gRPC 8001 and metrics 8002 (edge D2). Every host port lies in the agent's range
// and the host ports are pairwise distinct.
func portBindings(a Args) ([]map[string]any, error) {
	host, err := requiredInt(a, "host_port")
	if err != nil {
		return nil, err
	}
	if host < EdgeHostPortMin || host > EdgeHostPortMax {
		return nil, fmt.Errorf("host_port %d is outside the range %d-%d the agent may use on a device", host, EdgeHostPortMin, EdgeHostPortMax)
	}
	out := []map[string]any{binding(host, 8000)}
	seen := map[int64]bool{host: true}
	for _, opt := range []struct {
		name      string
		container int
	}{{"grpc_host_port", 8001}, {"metrics_host_port", 8002}} {
		n, ok, err := a.int(opt.name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if n < EdgeHostPortMin || n > EdgeHostPortMax {
			return nil, fmt.Errorf("%s %d is outside the range %d-%d the agent may use on a device", opt.name, n, EdgeHostPortMin, EdgeHostPortMax)
		}
		if seen[n] {
			return nil, fmt.Errorf("host port %d is given twice; every host port can be used once", n)
		}
		seen[n] = true
		out = append(out, binding(n, opt.container))
	}
	return out, nil
}

// binding is one port binding; the platform declares the ports as text.
func binding(host int64, container int) map[string]any {
	return map[string]any{
		"host_port":      strconv.FormatInt(host, 10),
		"container_port": strconv.Itoa(container),
		"protocol":       "tcp",
	}
}

// deploymentBody builds the body of a new deployment. build and the last step of the tool use this
// one function, so a plan step built with image_id is byte-identical to the later call (edge D2.5).
func deploymentBody(deviceID, imageID int64, name string, bindings []map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{
		"edge_device_id":     deviceID,
		"container_image_id": imageID,
		"name":               "agw-" + name,
		"port_bindings":      bindings,
	})
	return b, err
}

// createDeploymentSteps runs the pre-checks in a fixed order (device, image, architecture, ports,
// name) and only then sends the creation with the image's real identifier.
func createDeploymentSteps(ctx context.Context, a Args, _ Request, do Doer) (Result, string, error) {
	deviceID, _ := requiredInt(a, "device_id") // checked by build
	name, repo, tag, err := deploymentArgs(a)
	if err != nil {
		return Refuse("%s", err.Error())
	}
	bindings, err := portBindings(a)
	if err != nil {
		return Refuse("%s", err.Error())
	}
	providedID, err := optionalImageID(a)
	if err != nil {
		return Refuse("%s", err.Error())
	}

	var devices []edgeDeviceRec
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/edge-devices"}, &devices); stop {
		return res, "", err
	}
	now := time.Now()
	if res, unavailable := edgeUnavailable(devices, now); unavailable {
		return res, "", nil
	}
	var device *edgeDeviceRec
	for i := range devices {
		if devices[i].ID.String() == strconv.FormatInt(deviceID, 10) {
			device = &devices[i]
			break
		}
	}
	switch {
	case device == nil:
		return Refuse("%s is not in the platform's device list; {{tool:list_edge_devices}} lists the devices", "device "+strconv.FormatInt(deviceID, 10))
	case !device.Registered:
		return Refuse("%s is not registered with the platform (registered=false); a deployment on it would fail", deviceText(*device))
	case !DeviceOnline(device.LastHeartbeat, now):
		return Refuse("%s is offline (last heartbeat %s, more than %s ago); deploying would fail", deviceText(*device), heartbeatText(device.LastHeartbeat), EdgeOnlineWindow)
	}

	if repo == "" {
		// Only image_id is given: the registry list names the repository and the tag of that image.
		var all []imageEntry
		if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/container-images"}, &all); stop {
			return res, "", err
		}
		found := false
		for _, e := range all {
			if e.ID.String() == strconv.FormatInt(providedID, 10) {
				repo, tag, found = e.Repository, e.Tag, true
				break
			}
		}
		if !found || !repositoryRe.MatchString(repo) || !tagRe.MatchString(tag) {
			return Refuse("image %d is not in the platform's image list; check {{tool:container_image_status}}", providedID)
		}
	}
	image := repo + ":" + tag
	var entries []imageEntry
	{
		res, err := do(ctx, Request{Method: http.MethodGet, Path: "/container-images/" + image, Full: true})
		if err != nil {
			return res, "", err
		}
		if res.Status == "error" && res.HTTPStatus == http.StatusNotFound {
			return Refuse("image %s is not in the platform's registry; build it with {{tool:build_inference_image}} or check {{tool:container_image_status}}", image)
		}
		if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
			return res, "", nil
		}
		if err := json.Unmarshal([]byte(res.Body), &entries); err != nil {
			return Result{Status: "error", Message: fmt.Sprintf("GET /container-images/%s: response is not the expected JSON: %v", image, err)}, "", nil
		}
	}
	entry, ok := pickImage(entries, device.Arch)
	if !ok {
		return Refuse("image %s has no build for the device's architecture %s (the registry lists %s)", image, archText(device.Arch), platformsText(entries))
	}
	if providedID != 0 && entry.ID.String() != strconv.FormatInt(providedID, 10) {
		return Refuse("image_id %d does not belong to %s (the image there has id %s)", providedID, image, entry.ID)
	}
	if IsTrainingImage(entry.Tag, entry.ExposedPorts) {
		return Refuse("image %s is a training image, not an inference image", image)
	}

	for _, dep := range device.Deployments {
		if dep.Status == "undeployed" {
			continue
		}
		for _, used := range dep.PortBindings {
			for _, want := range bindings {
				if want["host_port"] == used.HostPort {
					return Refuse("host port %s is already used by deployment %s (%s) on %s; choose another port in %d-%d",
						used.HostPort, dep.ID, dep.Name, deviceText(*device), EdgeHostPortMin, EdgeHostPortMax)
				}
			}
		}
	}
	for _, dep := range device.Deployments {
		if dep.Name == "agw-"+name {
			return Refuse("a deployment named agw-%s already exists on %s (id %s); choose another name", name, deviceText(*device), dep.ID)
		}
	}

	imageID := providedID
	if imageID == 0 {
		n, err := strconv.ParseInt(entry.ID.String(), 10, 64)
		if err != nil {
			return Result{Status: "error", Message: "the registry answered with an image id the gateway cannot read: " + entry.ID.String()}, "", nil
		}
		imageID = n
	}
	body, err := deploymentBody(deviceID, imageID, name, bindings)
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, "", nil
	}
	res, err := do(ctx, Request{Method: http.MethodPost, Path: "/container-deployments", Body: body})
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, "", err
	}
	created := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(res.Body))
	dec.UseNumber()
	if dec.Decode(&created) == nil && created != nil {
		created["next"] = "start it with {{tool:start_deployment}} (approval; compute)"
		res.Body = jsonBody(created)
	}
	return res, "", nil
}

// edgeUnavailable answers with status unavailable when no registered device is online (G1.13, edge
// D2): the capability edge is missing, so the agent should not plan a deployment it cannot finish.
func edgeUnavailable(devices []edgeDeviceRec, now time.Time) (Result, bool) {
	for _, d := range devices {
		if d.Registered && DeviceOnline(d.LastHeartbeat, now) {
			return Result{}, false
		}
	}
	reason := "no edge device is registered"
	if len(devices) > 0 {
		reason = "no edge device is online (last heartbeat " + heartbeatText(lastHeartbeat(devices)) + ")"
	}
	return Result{Status: StatusUnavailable, Message: UnavailableText("edge", reason, EdgeWithout)}, true
}

// lastHeartbeat is the most recent heartbeat among the devices; empty when no device ever checked in.
func lastHeartbeat(devices []edgeDeviceRec) string {
	best := ""
	for _, d := range devices {
		if d.LastHeartbeat > best {
			best = d.LastHeartbeat
		}
	}
	return best
}

// heartbeatText names a heartbeat for a message; a device that never checked in says so.
func heartbeatText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

// pickImage chooses the registry entry that fits the device's architecture (edge D2). The registry
// may hold the same repository and tag for several architectures.
func pickImage(entries []imageEntry, arch string) (imageEntry, bool) {
	want := "linux/" + arch
	for _, e := range entries {
		if e.Platform == want {
			return e, true
		}
	}
	return imageEntry{}, false
}

// archText names the architecture of a device.
func archText(arch string) string {
	if arch == "" {
		return "unknown"
	}
	return arch
}

// platformsText lists the architectures the registry holds for the image.
func platformsText(entries []imageEntry) string {
	if len(entries) == 0 {
		return "none"
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Platform == "" {
			out = append(out, "unknown")
		} else {
			out = append(out, e.Platform)
		}
	}
	return strings.Join(out, ", ")
}

// start_deployment starts a deployment this chat created; the device pulls the image and runs the
// container with network access. Effect compute (edge D1.4).
var startDeploymentTool = Tool{
	Name: "start_deployment", CLI: "start-deployment",
	Desc: "Starts a deployment created in this chat: the edge device pulls the image (several GB the first time) and starts the container with network access. " +
		"Effect compute: the user approves every start on its own, except as a step of a plan that also builds the image ({{tool:build_inference_image}}, then {{tool:create_deployment}} with image_id \"{{stepN.id}}\", then this tool); it counts against the compute calls of the chat. " +
		"Response: the task (/tasks/<id>); follow it with {{tool:deployment_status}}.",
	Write:  true,
	Params: []Param{{Name: "deployment_id", Type: "integer", Desc: "id of the deployment ({{tool:list_deployments}} or the answer of {{tool:create_deployment}})", Required: true}},
	build:  byID(http.MethodPut, "/container-deployments", "deployment_id", "/deploy"),
	steps:  startDeploymentSteps,
}

func startDeploymentSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	id, _ := requiredInt(a, "deployment_id") // checked by build
	var dep deploymentRec
	{
		res, err := do(ctx, Request{Method: http.MethodGet, Path: "/container-deployments/" + strconv.FormatInt(id, 10), Full: true})
		if err != nil {
			return res, "", err
		}
		if res.Status == "error" && res.HTTPStatus == http.StatusNotFound {
			return Refuse("deployment %d is not in the platform; {{tool:list_deployments}} lists them", id)
		}
		if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
			return res, "", nil
		}
		if err := json.Unmarshal([]byte(res.Body), &dep); err != nil {
			return Result{Status: "error", Message: fmt.Sprintf("GET /container-deployments/%d: response is not the expected JSON: %v", id, err)}, "", nil
		}
	}
	if dep.Status == "deployed" {
		return Refuse("deployment %d (%q) is already deployed; the platform does not take it back without an undeploy (F4)", id, dep.Name)
	}
	var devices []edgeDeviceRec
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/edge-devices"}, &devices); stop {
		return res, "", err
	}
	now := time.Now()
	if res, unavailable := edgeUnavailable(devices, now); unavailable {
		return res, "", nil
	}
	for _, d := range devices {
		if d.ID.String() == dep.EdgeDeviceID.String() && !DeviceOnline(d.LastHeartbeat, now) {
			return Refuse("the device of deployment %d (%s) is offline (last heartbeat %s); the start would fail in the task",
				id, deviceText(d), heartbeatText(d.LastHeartbeat))
		}
	}
	res, err := do(ctx, final)
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, "", err
	}
	res.Body = jsonBody(map[string]any{
		"deployment_id": id,
		"task":          res.Location,
		"note":          "the platform starts the container in the background; the first start pulls the image (several GB). Follow the task with {{tool:deployment_status}}",
	})
	return res, "", nil
}

// deployment_status reports the state of a deployment, its device and, with task_id, the task that
// started it, with a hint to what the platform does not see (edge D2.6).
var deploymentStatusTool = Tool{
	Name: "deployment_status", CLI: "deployment-status",
	Desc:   "Returns the state of a deployment, its device and, with task_id, the task that started or undeployed it, with a hint what to do next (reads).",
	Params: []Param{{Name: "deployment_id", Type: "integer", Desc: "id of the deployment", Required: true}, {Name: "task_id", Type: "integer", Desc: "id of the task that started or undeployed it (from the Location of the start)"}},
	build:  byID(http.MethodGet, "/container-deployments", "deployment_id", ""),
	steps:  deploymentStatusSteps,
}

// The hints of deployment_status (edge D2.6, D2.4); a failed start leaves a container the platform
// cannot reach (F3), and the API never checks whether a started container still runs (F11).
const (
	msgStartFailed = "the start failed; a container named %s may be left on the device and the platform cannot remove it (no container id). Create a new deployment with another name and port; tell the user that the leftover container needs manual removal."
	msgNoCheck     = "the platform does not check whether the container still runs; deployed means it was started."
	msgDeviceLate  = "the device has not checked in for more than 5 minutes; deploying will fail."
)

// taskOut is the part of a platform task a status shows.
type taskOut struct {
	ID                   json.Number `json:"id"`
	Status               string      `json:"status"`
	CompletionPercentage json.Number `json:"completion_percentage,omitempty"`
	Message              string      `json:"message,omitempty"`
	CreatedAt            string      `json:"creation_date,omitempty"`
}

// statusOut is the answer of deployment_status.
type statusOut struct {
	DeploymentID json.Number `json:"deployment_id"`
	Name         string      `json:"name"`
	Status       string      `json:"status"`
	Device       struct {
		ID            json.Number `json:"id"`
		Name          string      `json:"name"`
		Online        bool        `json:"online"`
		LastHeartbeat string      `json:"last_heartbeat,omitempty"`
	} `json:"device"`
	Image       string   `json:"image,omitempty"`
	Ports       []string `json:"ports"`
	DockerIDSet bool     `json:"docker_container_id_present"`
	CreatedAt   string   `json:"created_at,omitempty"`
	Task        *taskOut `json:"task,omitempty"`
	Hint        string   `json:"hint,omitempty"`
}

func deploymentStatusSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	var dep deploymentRec
	if res, stop, err := ReadJSON(ctx, do, final, &dep); stop {
		return res, "", err
	}
	out := statusOut{
		DeploymentID: dep.ID, Name: dep.Name, Status: dep.Status,
		Image:       imageRef(dep.Image),
		Ports:       portsText(dep.PortBindings),
		DockerIDSet: dep.DockerContainerID != nil && *dep.DockerContainerID != "",
		CreatedAt:   dep.CreationDate,
	}
	out.Device.ID = dep.EdgeDeviceID

	var devices []edgeDeviceRec
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/edge-devices"}, &devices); stop {
		return res, "", err
	}
	now := time.Now()
	for _, d := range devices {
		if d.ID.String() == dep.EdgeDeviceID.String() {
			out.Device.Name = d.Name
			out.Device.Online = DeviceOnline(d.LastHeartbeat, now)
			out.Device.LastHeartbeat = d.LastHeartbeat
			break
		}
	}

	if tid, ok, err := a.int("task_id"); err != nil {
		return Result{Status: "error", Message: err.Error()}, "", nil
	} else if ok {
		var t taskOut
		if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/tasks/" + strconv.FormatInt(tid, 10)}, &t); stop {
			return res, "", err
		}
		out.Task = &t
	}

	switch {
	case out.Task != nil && out.Task.Status == "failed" && dep.Status == "created":
		out.Hint = fmt.Sprintf(msgStartFailed, dep.Name)
	case dep.Status == "deployed":
		out.Hint = msgNoCheck
	case !out.Device.Online && dep.Status != "undeployed":
		out.Hint = msgDeviceLate
	}
	return jsonResult(out)
}

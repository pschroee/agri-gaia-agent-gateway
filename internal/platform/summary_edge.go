// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// The summaries of the two writing calls of an edge deployment (edge D-E2, issue #72): creating a
// deployment and starting it. They live in their own file, so the switch in summary.go stays what it
// is; init appends the describer. Text of the agent never enters a summary.
func init() { registerDescriber(describeDeployment) }

var sumDeployPathRe = regexp.MustCompile(`^/container-deployments/([^/]+)/deploy$`)

// describeDeployment answers the deployment paths of its feature; a call it does not know keeps the
// built-in template.
func describeDeployment(c SummaryCall, s *summarizer) (string, []string, bool) {
	switch {
	case c.Method == http.MethodPost && c.Path == "/container-deployments":
		head, details := s.createDeployment()
		return head, details, true
	case c.Method == http.MethodPut && sumDeployPathRe.MatchString(c.Path):
		head, details := s.startDeployment(sumDeployPathRe.FindStringSubmatch(c.Path)[1])
		return head, details, true
	}
	return "", nil, false
}

// createDeployment writes the summary of a new deployment: which image on which device, with the
// name the gateway gives it and every host port it binds (edge D2.7).
func (s *summarizer) createDeployment() (string, []string) {
	for _, k := range []string{"edge_device_id", "container_image_id", "name", "port_bindings"} {
		if _, ok := s.body[k]; ok {
			s.usedBody[k] = true
		}
	}
	name, _ := s.body["name"].(string)
	if name == "" {
		name = "(no name)"
	} else {
		name = strconv.Quote(name)
	}
	head := fmt.Sprintf("Create deployment %s of %s on %s", name, s.imageText(scalarText(s.body["container_image_id"])), s.deviceText(scalarText(s.body["edge_device_id"])))
	details := []string{}
	if v, ok := s.body["port_bindings"]; ok {
		details = append(details, "Ports: "+portList(v))
	}
	details = append(details, "Nothing runs until the deployment is started.")
	return head, details
}

// startDeployment writes the summary of a start: the deployment by name and id, its image and the
// note that the container runs with network access and opens its ports on every interface of the
// device (edge D2.7). The deployment is read to name the image and the device.
func (s *summarizer) startDeployment(id string) (string, []string) {
	dep, err := s.object("/container-deployments/" + id)
	name, _ := dep["name"].(string)
	head := fmt.Sprintf("Start deployment %s (id %s)", strconv.Quote(name), id)
	if name == "" {
		head = "Start deployment " + id
	}
	head += " on " + s.deviceText(scalarText(dep["edge_device_id"]))
	details := []string{"Image: " + s.imageText(scalarText(dep["container_image_id"]))}
	if err != "" && s.read != nil {
		details = append(details, "The deployment could not be read ("+err+"); the image and the device are named by their ids only.")
	}
	details = append(details,
		"The device pulls the image and runs it with network access; ports are opened on all interfaces of the device.")
	return head, details
}

// deviceText names an edge device: "edge device \"sim-edge-01\" (id 1)". Without a readable list it
// stays with the id.
func (s *summarizer) deviceText(id string) string {
	if id == "" {
		return "an edge device (no id)"
	}
	if p, isPh := placeholder(id); isPh {
		return fmt.Sprintf("the edge device from step %d", p)
	}
	list, err := s.list("/edge-devices")
	for _, e := range list {
		m, _ := e.(map[string]any)
		if scalarText(m["id"]) == id {
			if name, _ := m["name"].(string); name != "" {
				return fmt.Sprintf("edge device %s (id %s)", strconv.Quote(name), id)
			}
		}
	}
	if err != "" && s.read != nil {
		return fmt.Sprintf("edge device %s (not readable: %s)", id, err)
	}
	return "edge device " + id
}

// imageText names a container image by its identifier: "\"agent-test/mnist-triton:v1\"". An id of a
// deployment created through the tool without image_id is 0 until the step runs; that is said instead
// of a wrong name (edge D2.5).
func (s *summarizer) imageText(id string) string {
	if id == "" || id == "0" {
		return "an image the tool resolves when the step runs"
	}
	if p, isPh := placeholder(id); isPh {
		return fmt.Sprintf("the image created by step %d", p)
	}
	list, err := s.list("/container-images")
	for _, e := range list {
		m, _ := e.(map[string]any)
		if scalarText(m["id"]) != id {
			continue
		}
		repo, _ := m["repository"].(string)
		tag, _ := m["tag"].(string)
		if repo != "" {
			return strconv.Quote(repo + ":" + tag)
		}
	}
	if err != "" && s.read != nil {
		return fmt.Sprintf("image %s (not readable: %s)", id, err)
	}
	return "image " + id
}

// portList renders the port bindings of a new deployment as "18100->8000/tcp, 18101->8001/tcp"; an
// unexpected shape is shown as it is, so nothing of the call stays hidden.
func portList(v any) string {
	list, ok := v.([]any)
	if !ok {
		return value(v)
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			parts = append(parts, value(e))
			continue
		}
		host := scalarText(m["host_port"])
		container := scalarText(m["container_port"])
		proto, _ := m["protocol"].(string)
		if host == "" && container == "" && proto == "" {
			parts = append(parts, fieldList(m))
			continue
		}
		parts = append(parts, host+"->"+container+"/"+proto)
	}
	return strings.Join(parts, ", ")
}

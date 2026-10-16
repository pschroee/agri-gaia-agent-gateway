// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

// The summaries of the two calls that take an edge deployment back (edge D-E3, issue #102): undeploy
// removes the container, deleting removes the deployment entry. They live in their own file, so the
// switch in summary.go and the templates of E2 stay what they are; init appends the describer. Text
// of the agent never enters a summary.
func init() { registerDescriber(describeDeploymentUndeploy) }

var (
	sumUndeployPathRe = regexp.MustCompile(`^/container-deployments/([^/]+)/undeploy$`)
	sumDeploymentRe   = regexp.MustCompile(`^/container-deployments/([^/]+)$`)
)

// describeDeploymentUndeploy answers the two paths of taking a deployment back; a call it does not
// know keeps the built-in template.
func describeDeploymentUndeploy(c SummaryCall, s *summarizer) (string, []string, bool) {
	switch {
	case c.Method == http.MethodPut && sumUndeployPathRe.MatchString(c.Path):
		head, details := s.undeployDeployment(sumUndeployPathRe.FindStringSubmatch(c.Path)[1])
		return head, details, true
	case c.Method == http.MethodDelete && sumDeploymentRe.MatchString(c.Path):
		head, details := s.deleteDeployment(sumDeploymentRe.FindStringSubmatch(c.Path)[1])
		return head, details, true
	}
	return "", nil, false
}

// undeployDeployment writes the summary of taking a deployment back: which deployment on which
// device, and what stays (edge D3.1, D3.3). The name is what the container removal alone does not
// say, so the deployment is read for it.
func (s *summarizer) undeployDeployment(id string) (string, []string) {
	dep, err := s.object("/container-deployments/" + id)
	name, _ := dep["name"].(string)
	head := fmt.Sprintf("Undeploy %s (id %s)", strconv.Quote(name), id)
	if name == "" {
		head = "Undeploy deployment " + id
	}
	head += " from " + s.deviceText(scalarText(dep["edge_device_id"]))
	details := []string{"The container and its anonymous volumes are removed; the deployment can be started again."}
	if err != "" && s.read != nil {
		details = append(details, "The deployment could not be read ("+err+"); the device is named by its id only.")
	}
	return head, details
}

// deleteDeployment writes the summary of removing a deployment entry: which one, and that nothing
// brings it back.
func (s *summarizer) deleteDeployment(id string) (string, []string) {
	dep, err := s.object("/container-deployments/" + id)
	name, _ := dep["name"].(string)
	head := fmt.Sprintf("Delete deployment %s (id %s)", strconv.Quote(name), id)
	if name == "" {
		head = "Delete deployment " + id
	}
	details := []string{"This cannot be undone."}
	if err != "" && s.read != nil {
		details = append(details, "The deployment could not be read ("+err+").")
	}
	return head, details
}

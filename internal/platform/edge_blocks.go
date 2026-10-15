// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"net/http"
	"regexp"
	"strings"
)

// Blocked edge paths (edge D-E1, D1.1): paths through which the agent could bring code onto edge
// devices or change the devices themselves. The reasons, each checked at the backend code:
//
//   - /applications: the compose file goes unchanged to Portainer as `StackFileContent` and is
//     deployed to every device of a group with administrator rights (routers/applications.py:73-90,
//     services/portainer/portainer_api.py:698-712). It is code whose content nobody can check.
//   - /model-deployments: the deployment service raises NotImplementedError
//     (services/model_deployment/deployment_service.py:22-33), POST only writes a database row
//     (routers/model_deployments.py:56-62). An agent must not be able to trigger that.
//   - POST /edge-devices: creates Portainer tags as administrator and hands out the device's
//     edge_key (routers/edge_devices.py:104-106); DELETE removes the endpoint in Portainer.
//   - /tags and /edge-groups: a tag decides which devices an application of a platform user lands
//     on; creating one is an administrator action. Reading them tells the agent nothing it needs to
//     change.
//
// The check runs in Normalize (registerBlocker), so the tool, the raw `request` and the REST binding
// go through it alike, and it holds with and without a delegation - the gateway never asks the user
// for a call it must not make at all.
func init() { registerBlocker(blockedEdgeCall) }

// The refusal messages (edge D-E1); the tests read them from here.
const (
	MsgApplicationsBlocked = "applications are blocked for the agent: a compose file is code that Portainer runs as administrator on edge devices"
	MsgModelDeployBlocked  = "model deployments are blocked for the agent: the platform does not implement them"
	MsgEdgeReadOnly        = "edge groups, tags and edge devices can only be read by the agent"
)

// deploymentListRe matches the one reading path below /edge-devices/ that stays open: the deployments
// of a device.
var deploymentListRe = regexp.MustCompile(`^/edge-devices/[^/]+/deployments$`)

// blockedEdgeCall returns the reason that keeps this call from the agent, or "" when it may go. The
// path is compared in lower case, because the platform's paths are case sensitive but not uniformly
// spelled in the tools and in the frontend.
func blockedEdgeCall(r Request) string {
	p := strings.ToLower(r.Path)
	switch {
	case p == "/applications" || strings.HasPrefix(p, "/applications/"):
		return MsgApplicationsBlocked
	case p == "/model-deployments" || strings.HasPrefix(p, "/model-deployments/"):
		return MsgModelDeployBlocked
	case p == "/tags" || strings.HasPrefix(p, "/tags/"):
		if r.Method == http.MethodGet {
			return ""
		}
		return MsgEdgeReadOnly
	case p == "/edge-groups" || strings.HasPrefix(p, "/edge-groups/"):
		if r.Method == http.MethodGet {
			return ""
		}
		return MsgEdgeReadOnly
	case p == "/edge-devices":
		if r.Method == http.MethodGet {
			return ""
		}
		return MsgEdgeReadOnly
	case strings.HasPrefix(p, "/edge-devices/"):
		// GET /edge-devices/{id}/deployments lists what runs on the device and stays readable. The
		// writing GET /edge-devices/{id} is not: the backend registers the device with Portainer
		// there, under a name the agent can match (Review 5, W5).
		if r.Method == http.MethodGet && deploymentListRe.MatchString(p) {
			return ""
		}
		return MsgEdgeReadOnly
	}
	return ""
}

// IsTrainingImage reports whether an image is for training, not for inference (edge D1.10): its tag
// ends on "-train" or it exposes Jupyter (8888) or TensorBoard (6006). The backend knows no such
// attribute (model_id is empty on every image of the instance and owner is the pushing account),
// so the gateway decides from the data the image list gives. Heuristic, seen on the instance:
// mnist-train, ethz-food101-train and the Jupyter template expose 8888/tcp.
func IsTrainingImage(tag string, exposedPorts []string) bool {
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(tag)), "-train") {
		return true
	}
	for _, p := range exposedPorts {
		switch strings.TrimSuffix(strings.TrimSpace(p), "/tcp") {
		case "8888", "6006":
			return true
		}
	}
	return false
}

// Edge host ports a deployment of the agent may bind on the device (edge D1.5). On a device that
// carries other services (fake-edge-01 is the platform host), a fixed free range keeps a deployment
// from hiding a service or taking a port somebody else holds.
const (
	EdgeHostPortMin = 18100
	EdgeHostPortMax = 18199
)

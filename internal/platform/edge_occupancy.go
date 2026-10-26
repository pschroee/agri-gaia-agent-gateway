// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// What is taken on an edge device (issue #231): the name and the host ports of a new deployment are checked
// against the deployments the platform knows and the containers that really are on the device. The same rule
// runs when a plan with a deployment is proposed and when create_deployment runs, and it writes the same messages.
// The message formats are quoted by tests, the plan and API.md.
const (
	MsgNameTakenDeployment = "name %s is taken on %s by deployment %s (status %s); use a free name, e.g. name %q"
	MsgNameTakenContainer  = "a container named %s is on %s (state %s) and belongs to no platform deployment; use a free name, e.g. name %q, and tell the user that this container needs removal on the device"
	MsgPortTakenDeployment = "host port %s is used by deployment %s (%s) on %s; free host ports: %s"
	MsgPortTakenContainer  = "host port %s is published by container %s on %s; free host ports: %s"
	MsgNameInPlan          = "name %s is also given to step %d of this plan on the same device; use a free name, e.g. name %q"
	MsgPortInPlan          = "host port %s is also used by step %d of this plan on the same device; free host ports: %s"
	MsgContainersUnread    = "the containers on %s could not be read (%s); only the platform's deployments were checked"

	// NoFreePorts is the list of free ports when the whole range is taken.
	NoFreePorts = "none in 18100-18199"

	// occupancyFreePorts is how many free ports a refusal names.
	occupancyFreePorts = 3
	// occupancyNameMax is the longest container name the platform accepts (deploymentNameRe in delegation).
	occupancyNameMax = 62
	// occupancyReadTimeout bounds the read of the containers, so that a device without connection does not hold up a proposal.
	occupancyReadTimeout = 20 * time.Second
)

// deviceContainer is one container on a device as GET /edge-devices/{id}/containers lists it.
type deviceContainer struct {
	Name      string        `json:"name"`
	State     string        `json:"state"`
	HostPorts []json.Number `json:"host_ports"`
}

// occupiedDeployment is a deployment entry of the device as far as the check needs it.
type occupiedDeployment struct {
	ID     string
	Name   string
	Status string
	Ports  []string
}

// reservation is what an earlier step of the plan will take on the device.
type reservation struct {
	Step  int
	Name  string // with the agw- prefix
	Ports []string
}

// Occupancy is what is taken on one device. Note is set when the containers could not be read.
type Occupancy struct {
	device      edgeDeviceRec
	deployments []occupiedDeployment
	containers  []deviceContainer
	reserved    []reservation
	Note        string
}

// occupancyOf starts from the deployments the platform lists for the device.
func occupancyOf(d edgeDeviceRec) Occupancy {
	o := Occupancy{device: d}
	for _, dep := range d.Deployments {
		od := occupiedDeployment{ID: dep.ID.String(), Name: dep.Name, Status: dep.Status}
		for _, p := range dep.PortBindings {
			od.Ports = append(od.Ports, p.HostPort)
		}
		o.deployments = append(o.deployments, od)
	}
	return o
}

// readContainers adds the containers of the device. When they cannot be read (route missing, device not
// registered, Portainer down, timeout), the check goes on with the platform's deployments and Note says so (E7).
func (o *Occupancy) readContainers(ctx context.Context, do Doer, deviceID string) {
	ctx, cancel := context.WithTimeout(ctx, occupancyReadTimeout)
	defer cancel()
	var list []deviceContainer
	res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/edge-devices/" + deviceID + "/containers"}, &list)
	if err != nil || stop {
		reason := "no answer"
		switch {
		case err != nil:
			reason = err.Error()
		case res.Message != "":
			reason = res.Message
		case res.HTTPStatus != 0:
			reason = "HTTP " + strconv.Itoa(res.HTTPStatus)
		}
		if r := []rune(reason); len(r) > 150 {
			reason = string(r[:150]) + "…"
		}
		o.Note = fmt.Sprintf(MsgContainersUnread, deviceText(o.device), reason)
		return
	}
	o.containers = list
}

// ReadOccupancy reads the device and its containers. ok is false when the device list cannot be read or does
// not hold the device; a failed read of the containers only sets Note.
func ReadOccupancy(ctx context.Context, do Doer, deviceID string) (Occupancy, bool) {
	var devices []edgeDeviceRec
	if _, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/edge-devices"}, &devices); stop || err != nil {
		return Occupancy{}, false
	}
	for _, d := range devices {
		if d.ID.String() == deviceID {
			o := occupancyOf(d)
			o.readContainers(ctx, do, deviceID)
			return o, true
		}
	}
	return Occupancy{}, false
}

// deploymentNamed finds the deployment entry with this (prefixed) name.
func (o Occupancy) deploymentNamed(full string) *occupiedDeployment {
	for i := range o.deployments {
		if o.deployments[i].Name == full {
			return &o.deployments[i]
		}
	}
	return nil
}

// portTaken lists who holds a host port: kind "deployment" (entry not undeployed), "container" (published by a
// container that belongs to no live entry) or "plan" (an earlier step). A container with the name of a live
// entry counts as that entry and is not named twice.
func (o Occupancy) portHolder(port string) (kind string, dep *occupiedDeployment, container string, step int) {
	for i := range o.deployments {
		if o.deployments[i].Status == "undeployed" {
			continue
		}
		for _, p := range o.deployments[i].Ports {
			if p == port {
				return "deployment", &o.deployments[i], "", 0
			}
		}
	}
	for _, c := range o.containers {
		if d := o.deploymentNamed(c.Name); d != nil && d.Status != "undeployed" {
			continue
		}
		for _, p := range c.HostPorts {
			if p.String() == port {
				return "container", nil, c.Name, 0
			}
		}
	}
	for _, r := range o.reserved {
		for _, p := range r.Ports {
			if p == port {
				return "plan", nil, "", r.Step
			}
		}
	}
	return "", nil, "", 0
}

// nameTaken tells whether a (prefixed) name is taken, by a deployment entry, a container or an earlier step.
func (o Occupancy) nameTaken(full string) bool {
	if o.deploymentNamed(full) != nil {
		return true
	}
	for _, c := range o.containers {
		if c.Name == full {
			return true
		}
	}
	for _, r := range o.reserved {
		if r.Name == full {
			return true
		}
	}
	return false
}

// Conflicts lists what keeps a deployment with this name (without the agw- prefix) and these host ports from
// being created on the device: names first, then the ports in the order given. Each message names a free value.
func (o Occupancy) Conflicts(name string, hostPorts []string) []string {
	var out []string
	full := "agw-" + name
	where := deviceText(o.device)
	if d := o.deploymentNamed(full); d != nil {
		out = append(out, fmt.Sprintf(MsgNameTakenDeployment, full, where, d.ID, deployStatusText(d.Status), o.FreeName(name)))
	} else if c, ok := o.containerNamed(full); ok {
		out = append(out, fmt.Sprintf(MsgNameTakenContainer, full, where, c.State, o.FreeName(name)))
	} else {
		for _, r := range o.reserved {
			if r.Name == full {
				out = append(out, fmt.Sprintf(MsgNameInPlan, full, r.Step, o.FreeName(name)))
				break
			}
		}
	}
	for _, p := range hostPorts {
		kind, dep, container, step := o.portHolder(p)
		switch kind {
		case "deployment":
			out = append(out, fmt.Sprintf(MsgPortTakenDeployment, p, dep.ID, dep.Name, where, o.FreePorts(hostPorts)))
		case "container":
			out = append(out, fmt.Sprintf(MsgPortTakenContainer, p, container, where, o.FreePorts(hostPorts)))
		case "plan":
			out = append(out, fmt.Sprintf(MsgPortInPlan, p, step, o.FreePorts(hostPorts)))
		}
	}
	return out
}

// containerNamed finds a container with this name.
func (o Occupancy) containerNamed(full string) (deviceContainer, bool) {
	for _, c := range o.containers {
		if c.Name == full {
			return c, true
		}
	}
	return deviceContainer{}, false
}

// Reserve records what the create step of a plan takes, so that later steps see it (E9). name is the
// argument form without the agw- prefix.
func (o *Occupancy) Reserve(step int, name string, hostPorts []string) {
	o.reserved = append(o.reserved, reservation{Step: step, Name: "agw-" + name, Ports: append([]string(nil), hostPorts...)})
}

// Release gives back what an earlier undeploy (all = false: the ports of the entry and its container) or
// delete_deployment (all = true: the whole entry, so the name too) frees (E9). An unknown id changes nothing.
func (o *Occupancy) Release(deploymentID string, all bool) {
	for i := range o.deployments {
		if o.deployments[i].ID != deploymentID {
			continue
		}
		name := o.deployments[i].Name
		kept := o.containers[:0:0]
		for _, c := range o.containers {
			if c.Name != name {
				kept = append(kept, c)
			}
		}
		o.containers = kept
		if all {
			o.deployments = append(o.deployments[:i:i], o.deployments[i+1:]...)
		} else {
			o.deployments[i].Status = "undeployed"
		}
		return
	}
}

// FreeName is the first of name-2 … name-99 that is free; the base is cut so that agw-<name> has at most 62
// characters. name has no agw- prefix.
func (o Occupancy) FreeName(name string) string {
	first := ""
	for n := 2; n <= 99; n++ {
		suffix := "-" + strconv.Itoa(n)
		base := name
		if max := occupancyNameMax - len("agw-") - len(suffix); len(base) > max {
			base = strings.TrimRight(base[:max], "-._")
		}
		cand := base + suffix
		if first == "" {
			first = cand
		}
		if !o.nameTaken("agw-" + cand) {
			return cand
		}
	}
	return first
}

// FreePorts names the three lowest free host ports of the agent's range, without those in exclude (the other
// ports of the same call). The list is "18101, 18102, 18103", or NoFreePorts.
func (o Occupancy) FreePorts(exclude []string) string {
	skip := map[string]bool{}
	for _, p := range exclude {
		skip[p] = true
	}
	var free []string
	for p := EdgeHostPortMin; p <= EdgeHostPortMax && len(free) < occupancyFreePorts; p++ {
		s := strconv.Itoa(p)
		if skip[s] {
			continue
		}
		if kind, _, _, _ := o.portHolder(s); kind != "" {
			continue
		}
		free = append(free, s)
	}
	if len(free) == 0 {
		return NoFreePorts
	}
	return strings.Join(free, ", ")
}

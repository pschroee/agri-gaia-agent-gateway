// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// The undeploy and delete steps with a literal deployment id (#231, E9).
var (
	undeployPathRe = regexp.MustCompile(`^/container-deployments/([0-9]+)/undeploy$`)
	deletePathRe   = regexp.MustCompile(`^/container-deployments/([0-9]+)$`)
)

// release is a step that gives back a deployment's ports (undeploy) or the whole entry (delete).
type release struct {
	id  string
	all bool
}

// deploymentStepProblems checks the deployments a plan creates against the device when the plan is proposed
// (#231, E5-E9): name and host ports must be free on the device, neither taken by a platform deployment nor by a
// container there nor by an earlier step of the plan. A problem names a free name and free ports and is joined
// with the other problems of the proposal; no approval is created for a refused plan. An earlier undeploy or
// delete step with a literal id gives back what it frees. A device that cannot be read is skipped (E7).
func (m *Manager) deploymentStepProblems(ctx context.Context, chatID string, steps []store.PlanStep) error {
	do := func(c context.Context, r platform.Request) (platform.Result, error) {
		res, err := m.opt.Platform.Do(c, chatID, r)
		result := "ok"
		if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
			result = "error"
		}
		m.LogCallBy(c, m.slotOf(chatID), chatID, "orchestrator", "edge_occupancy_read", "GET "+r.Path, result)
		return res, err
	}
	devices := map[string]*platform.Occupancy{}
	unreadable := map[string]bool{}
	var released []release
	var problems plan.Problems
	for _, st := range steps {
		switch {
		case st.Method == http.MethodPut && undeployPathRe.MatchString(st.Path):
			released = append(released, release{id: undeployPathRe.FindStringSubmatch(st.Path)[1]})
		case st.Method == http.MethodDelete && deletePathRe.MatchString(st.Path):
			released = append(released, release{id: deletePathRe.FindStringSubmatch(st.Path)[1], all: true})
		default:
			if !plan.IsCreateDeploymentStep(st) {
				continue
			}
			deviceID, name, ports, ok := deploymentStepFields(st.Body)
			if !ok || unreadable[deviceID] {
				continue
			}
			occ := devices[deviceID]
			if occ == nil {
				read, ok := platform.ReadOccupancy(ctx, do, deviceID)
				if !ok {
					unreadable[deviceID] = true
					slog.Warn("edge occupancy: device not readable, step not checked", "chat", chatID, "device", deviceID, "step", st.Idx)
					continue
				}
				if read.Note != "" {
					slog.Warn("edge occupancy: containers not readable", "chat", chatID, "device", deviceID, "note", read.Note)
				}
				occ = &read
				for _, r := range released {
					occ.Release(r.id, r.all)
				}
				devices[deviceID] = occ
			}
			for _, msg := range occ.Conflicts(name, ports) {
				problems = append(problems, plan.Problem{Step: st.Idx, Msg: msg})
			}
			occ.Reserve(st.Idx, name, ports)
			continue
		}
		last := released[len(released)-1]
		for _, occ := range devices {
			occ.Release(last.id, last.all)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return problems
}

// deploymentStepFields reads device, name (without the agw- prefix) and host ports of a create-deployment step.
// ok is false when the device is not a literal id or the name has no agw- prefix: the field rules of the proposal
// report the latter, and a placeholder cannot be looked up.
func deploymentStepFields(body json.RawMessage) (deviceID, name string, hostPorts []string, ok bool) {
	var b struct {
		Device json.Number `json:"edge_device_id"`
		Name   string      `json:"name"`
		Ports  []struct {
			HostPort json.Number `json:"host_port"`
		} `json:"port_bindings"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if d.Decode(&b) != nil {
		return "", "", nil, false
	}
	deviceID = b.Device.String()
	if deviceID == "" || !digitsOnly(deviceID) || !strings.HasPrefix(b.Name, "agw-") {
		return "", "", nil, false
	}
	for _, p := range b.Ports {
		hostPorts = append(hostPorts, p.HostPort.String())
	}
	return deviceID, strings.TrimPrefix(b.Name, "agw-"), hostPorts, true
}

func digitsOnly(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

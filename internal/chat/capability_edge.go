// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"agw/internal/platform"
)

// The capability "edge" (edge D-E2, G1.11): whether this setup has an edge device the agent could
// deploy an inference image to. The entry lives in its own file; the state comes from the platform's
// device list, read with the user's token and kept for a minute (G1.11).
func init() { registerCapability("edge", capabilityEdge) }

// edgeDeviceState is the part of an edge device the entry reads. The heartbeat is a naive UTC
// timestamp without a zone in the JSON (F8); platform.DeviceOnline reads it.
type edgeDeviceState struct {
	Name          string   `json:"name"`
	Arch          string   `json:"arch"`
	Tags          []string `json:"tags"`
	Registered    bool     `json:"registered"`
	LastHeartbeat string   `json:"last_heartbeat"`
}

// capabilityEdge: is at least one registered edge device online (heartbeat within
// platform.EdgeOnlineWindow)? Changeable during a chat, so the platform is read once a minute.
func capabilityEdge(ctx context.Context, m *Manager, chatID string) Capability {
	c := Capability{Without: platform.EdgeWithout}
	if m.opt.Platform == nil {
		c.Detail = "the platform binding of this gateway is off"
		return c
	}
	return m.cachedCapability(chatID, "edge", func() Capability {
		res, err := m.opt.Platform.Do(ctx, chatID, platform.Request{Method: http.MethodGet, Path: "/edge-devices"})
		if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
			c.Detail = "the edge devices are not readable right now"
			return c
		}
		var devices []edgeDeviceState
		if json.Unmarshal([]byte(res.Body), &devices) != nil {
			c.Detail = "the platform answered in a form the gateway does not know"
			return c
		}
		now := time.Now()
		online := make([]edgeDeviceState, 0, len(devices))
		last := ""
		for _, d := range devices {
			if d.Registered && platform.DeviceOnline(d.LastHeartbeat, now) {
				online = append(online, d)
			}
			if d.LastHeartbeat > last {
				last = d.LastHeartbeat
			}
		}
		if len(online) == 0 {
			switch {
			case len(devices) == 0:
				c.Detail = "no edge device is registered"
			case last == "":
				c.Detail = "no edge device online (no heartbeat)"
			default:
				c.Detail = fmt.Sprintf("no edge device online (last heartbeat %s)", last)
			}
			return c
		}
		c.Available = true
		c.Detail = edgeDeviceSummary(online)
		return c
	})
}

// edgeDeviceSummary names the online devices: "1 device online: sim-edge-01 (simulated, amd64)".
// A device marked with the tag simulated runs in a container on the platform host (edge E0).
func edgeDeviceSummary(online []edgeDeviceState) string {
	parts := make([]string, 0, len(online))
	for _, d := range online {
		kinds := make([]string, 0, 2)
		if slices.Contains(d.Tags, "simulated") {
			kinds = append(kinds, "simulated")
		}
		if d.Arch != "" {
			kinds = append(kinds, d.Arch)
		}
		if len(kinds) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", d.Name, strings.Join(kinds, ", ")))
		} else {
			parts = append(parts, d.Name)
		}
	}
	word := "devices"
	if len(online) == 1 {
		word = "device"
	}
	return fmt.Sprintf("%d %s online: %s", len(online), word, strings.Join(parts, ", "))
}

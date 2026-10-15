// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"agw/internal/platform"
)

// Pre-annotation with CVAT (V1): the entry annotation of the overview (G1.8). It is available when the
// service is wired — the doer answers the prefix /_agw/annotation — and CVAT itself answers; the number
// of usable functions comes from the same listing the tool annotation_functions gives the agent.
func init() { registerCapability("annotation", capabilityAnnotation) }

func capabilityAnnotation(ctx context.Context, m *Manager, chatID string) Capability {
	c := Capability{Without: "pre-annotation is not available here; the user annotates in CVAT by hand (Annotate button of the dataset)"}
	d, ok := m.virtualFor(platform.AnnotationPrefix)
	if !ok {
		c.Detail = "the annotation service is not part of this setup"
		return c
	}
	// The listing reads CVAT, so the answer is kept for a moment (G1.11).
	return m.cachedCapability(chatID, "annotation", func() Capability {
		res, err := d.Do(ctx, chatID, platform.Request{Method: http.MethodGet, Path: platform.AnnotationPrefix + "/functions"})
		if err != nil {
			c.Detail = "CVAT is not reachable right now"
			return c
		}
		if res.Status != "ok" {
			c.Detail = "pre-annotation is not usable right now: " + res.Message
			return c
		}
		var answer struct {
			Functions []json.RawMessage `json:"functions"`
		}
		if json.Unmarshal([]byte(res.Body), &answer) != nil {
			c.Detail = "CVAT answered in a form the gateway does not know"
			return c
		}
		c.Available = true
		c.Detail = fmt.Sprintf("CVAT reachable, %d usable pre-annotation functions", len(answer.Functions))
		return c
	})
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"net/http"
)

// PlanPath is the path under which the orchestrator takes a plan proposal (issue #70); it
// never goes to the platform.
const PlanPath = "/_agw/plan"

// PlanToolDescription is the text of propose_plan for the agent (CLI, MCP and REST alike).
const PlanToolDescription = `Proposes a plan of several writing platform calls for ONE approval by the user. Use it when a task needs two or more writing calls in a row (e.g. create a model from a training, then build an image from it). plan = {"title": "short title", "steps": [...]}; a step is either {"tool": "<platform tool without prefix, e.g. start_training>", "args": {...}} or {"method": "POST", "path": "/...", "query": {...}, "body": {...}}, each optionally with "description". At most 10 steps. The id of an object an earlier step creates is written as the string "{{stepN.id}}" (N = number of that step, from 1), as a whole argument, body value or path segment. Reading calls do not belong in a plan. After the approval, run the steps in exactly this order with exactly these parameters: each matching call then runs without a new approval for 30 minutes. Steps marked irreversible (deleting, publishing) still ask the user. Any other or changed call needs its own approval.`

// planTool registers propose_plan for CLI, MCP and REST (the table of tools is the single source).
var planTool = Tool{
	Name: "propose_plan",
	CLI:  "propose-plan",
	Desc: PlanToolDescription,
	Params: []Param{
		{Name: "plan", Type: "object", Desc: `{"title": "short title", "steps": [...]}, at most 10 steps`, Required: true},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		body, err := json.Marshal(a["plan"])
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: PlanPath, Body: body}, nil
	},
}

func init() { Tools = append(Tools, planTool) }

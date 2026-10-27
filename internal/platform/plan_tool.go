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

// PlanToolDescription is the text of propose_plan for the agent (CLI, MCP and REST alike). The list of the
// tools that can be steps is every writing tool but propose_plan, the list of the tools that create an object
// those whose call the delegation classifies as create (tests keep both true, issue #218).
const PlanToolDescription = `Proposes a plan of several writing platform calls for ONE approval by the user. Use it when a task needs two or more writing calls in a row, and propose ONE plan with every step you know. plan is a JSON object (the input schema has every field): {"title", "steps": [...], optional "mode", "budget", "replaces"}. A step is either {"tool": "<tool name without prefix>", "args": {...}} with exactly that tool's arguments, or a raw call {"method", "path", "query", "body"}; each may have "description" (at most 200 characters). Tools that can be steps: create_training, start_training, upload_dataset, upload_model, annotation_task, pre_annotate, annotation_sync, annotation_function, annotation_check, annotation_fill, annotation_rollback, split_dataset, assess_model, create_deployment, start_deployment, undeploy, delete_deployment, import_template, build_inference_image, rename_dataset, update_model, create_model_from_training, notebook_run, notebook_stop, propose_search, search_next, end_search, clone_training. Reading calls do not belong in a plan. At most 10 steps. The id of an object an earlier step creates is the string "{{stepN.id}}" (N from 1), as a whole argument, body value or path segment. Steps that create an object: upload_dataset, upload_model, split_dataset, create_training, clone_training, create_model_from_training, build_inference_image, create_deployment, import_template; for create_training and build_inference_image it is the training container and the image the job creates, and the next step runs after that job has ended. Point a placeholder at the step that created the object, never at one that only starts or changes it (start_training, start_deployment, update_model). Example: split_dataset → create_training with dataset_id "{{step1.id}}" → start_training and then create_model_from_training with train_container_id "{{step2.id}}" → update_model with model_id "{{step4.id}}" and only "name" (the tool reads the model when the step runs and keeps its other fields; required before a build, the inference server cannot address the training's name) → build_inference_image with model_id "{{step4.id}}" → create_deployment with image_id "{{step6.id}}" and no repository or tag → start_deployment with deployment_id "{{step7.id}}". For uploads give the files only: the gateway reads them and pins their SHA-256. After the approval, run the steps in exactly this order with exactly these parameters: each matching call then runs without a new approval for 30 minutes. Steps marked irreversible (deleting, publishing) still ask the user. Any other or changed call needs its own approval. For a long task the user wants done without approving each call, set "mode": "autonomous" and a "budget" {"max_duration_minutes", "max_llm_cost_usd", "max_auto_turns" (1-50)}; duration and cost at most the limits {{tool:capabilities}} names under "plans"; a step may set "max_attempts" (1-3). While an autonomous plan runs, every call outside the rights derived from its steps is refused without asking; to change course, propose a new plan with "replaces": "<plan_id>". Irreversible steps, notebook runs and a start_deployment whose image the plan does not build still ask the user; to deploy with one approval, build the image in the same plan and give create_deployment "image_id": "{{stepN.id}}" of the build step. Before proposing steps that need notebooks, edge devices, pre-annotation or images, call {{tool:capabilities}}: plan only with what this setup offers and say what you leave out; there is enough compute for the plan only if capabilities shows that many compute calls left. A refused proposal lists every problem found: fix all of them, then propose the whole plan once more.`

// planSchema is the JSON Schema of the plan (issue #218): the MCP tool carries it as the schema of the
// argument "plan". Limits and unknown fields are left to the gateway and only named in the descriptions: pi checks
// MCP arguments against the schema before the gateway sees them and would answer with a limit alone, hiding the
// other problems the gateway lists in one answer (live, 26.10.2026). The CLI help and the gateway index print it. The fields
// file_sha256, file_bytes and file_error of a step are written by the gateway and are not part of it.
var planSchema = map[string]any{
	"type":     "object",
	"required": []string{"title", "steps"},
	"properties": map[string]any{
		"title": map[string]any{"type": "string", "description": "short title of the plan, 1 to 120 characters"},
		"steps": map[string]any{
			// A JSON text of the array is parsed by the gateway (#218); a client that checks the schema first
			// (pi over MCP) must let it through, so the type names both.
			"type":        []string{"array", "string"},
			"description": "the steps in the order they run, 1 to 10 (an array; a JSON text of it is parsed too)",
			"items": map[string]any{
				"type":        "object",
				"description": "either tool with args, or method with path (query, body)",
				"properties": map[string]any{
					"tool":         map[string]any{"type": "string", "description": "tool name without prefix, e.g. start_training"},
					"args":         map[string]any{"type": "object", "description": "the arguments of the tool"},
					"method":       map[string]any{"type": "string", "enum": []string{"POST", "PUT", "PATCH", "DELETE"}, "description": "method of a raw call"},
					"path":         map[string]any{"type": "string", "description": "path of a raw call, e.g. /train/config"},
					"query":        map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "query of a raw call"},
					"body":         map[string]any{"description": "JSON body of a raw call"},
					"description":  map[string]any{"type": "string", "description": "what the step does, at most 200 characters"},
					"max_attempts": map[string]any{"type": "integer", "description": "autonomous plan only: how often to try the step, 1 to 3"},
				},
			},
		},
		"mode": map[string]any{"type": "string", "enum": []string{"interactive", "autonomous"}, "description": "interactive (default) or autonomous"},
		"budget": map[string]any{
			"type":        "object",
			"description": "autonomous plan only; at most the limits capabilities names under plans",
			"properties": map[string]any{
				"max_duration_minutes": map[string]any{"type": "integer", "description": "how long the plan may run, at least 1"},
				"max_llm_cost_usd":     map[string]any{"type": "number", "description": "what the model calls of the plan may cost, more than 0"},
				"max_auto_turns":       map[string]any{"type": "integer", "description": "turns without the user, 1 to 50"},
			},
		},
		"replaces": map[string]any{"type": "string", "description": "id of the running plan of this chat that this one replaces"},
	},
}

// planTool registers propose_plan for CLI, MCP and REST (the table of tools is the single source).
var planTool = Tool{
	Name: "propose_plan",
	CLI:  "propose-plan",
	Desc: PlanToolDescription,
	Params: []Param{
		{Name: "plan", Type: "object", Desc: `{"title": "short title", "steps": [...]}, at most 10 steps`, Required: true, Schema: planSchema},
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

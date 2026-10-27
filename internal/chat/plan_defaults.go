// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// derivedDefault is a field the gateway's own handler fills when a call leaves it out (issue #236). The plan match
// accepts a call that names exactly that value as the step that leaves it out (and the other way round).
//
// Rule for an entry: the key is the path of a call the gateway answers itself (/_agw/...), and the handler of that
// path fills the field with the same function that derive uses. Defaults of the platform, of the setup or of a
// device are not entered: the gateway cannot vouch for them.
type derivedDefault struct {
	field  string
	number bool // the field is a JSON number (an id), not a string
	derive func(ctx context.Context, m *Manager, chatID string, read platform.Doer, body json.RawMessage) (string, bool)
}

var derivedDefaults = map[string]derivedDefault{
	platform.AssessSplitPath: {field: "name", derive: splitDefaultName},
	// B19 (demo 5 of 27.10.2026): the final test of a plan leaves eval_dataset_id out, the agent names the source of
	// the split explicitly – the very dataset the check handler takes without it (assess.defaultEvalDataset).
	platform.AssessCheckPath: {field: "eval_dataset_id", number: true, derive: checkDefaultEvalDataset},
}

// splitDefaultName reads the source dataset of a split call and returns the name the handler gives the new one.
func splitDefaultName(ctx context.Context, _ *Manager, _ string, read platform.Doer, body json.RawMessage) (string, bool) {
	var b struct {
		DatasetID json.Number `json:"dataset_id"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return "", false
	}
	id, err := strconv.Atoi(b.DatasetID.String())
	if err != nil || id <= 0 {
		return "", false
	}
	var src struct {
		Name string `json:"name"`
	}
	_, stop, err := platform.ReadJSON(ctx, read, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.Itoa(id)}, &src)
	if err != nil || stop || strings.TrimSpace(src.Name) == "" {
		return "", false
	}
	return platform.SplitDefaultName(src.Name), true
}

// checkDefaultEvalDataset is the dataset a check of a platform model measures on when the call names none: the source
// of the split whose training dataset the model saw, else that training dataset (the rule of
// assess.Service.defaultEvalDataset, read from the same rows). A check of an own file has no such default.
func checkDefaultEvalDataset(ctx context.Context, m *Manager, chatID string, _ platform.Doer, body json.RawMessage) (string, bool) {
	var b struct {
		ModelID json.Number `json:"model_id"`
		Path    string      `json:"path"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.Path != "" {
		return "", false
	}
	id, err := strconv.ParseInt(b.ModelID.String(), 10, 64)
	if err != nil || id <= 0 || m.st == nil {
		return "", false
	}
	o, err := m.st.ModelOriginOf(ctx, chatID, id)
	if err != nil || len(o.SeenDatasetIDs) == 0 {
		return "", false
	}
	for _, seen := range o.SeenDatasetIDs {
		if sp, err := m.st.SplitOfTrainDataset(ctx, chatID, seen); err == nil && sp.SourceDatasetID > 0 {
			return strconv.FormatInt(sp.SourceDatasetID, 10), true
		}
	}
	return strconv.FormatInt(o.SeenDatasetIDs[0], 10), true
}

// matchDerived tells whether req is the raw step cur but for a derived default; it returns the call in the form of
// the step. Any failure (no entry, no reader, failed read, other difference) is false: the call asks as before.
func (m *Manager) matchDerived(ctx context.Context, chatID string, cur store.PlanStep, req platform.Request, resolve plan.Resolve) (platform.Request, bool) {
	if req.Method != http.MethodPost || plan.IsToolStep(cur) {
		return req, false
	}
	d, ok := derivedDefaults[req.Path]
	if !ok {
		return req, false
	}
	read := m.summaryReader(chatID)
	if read == nil {
		return req, false
	}
	// The source is named by the call; the step's form of it agrees (WithDerived re-checks the whole call).
	value, ok := d.derive(ctx, m, chatID, read, req.Body)
	if !ok {
		return req, false
	}
	return plan.WithDerivedValue(cur, req, resolve, d.field, value, d.number)
}

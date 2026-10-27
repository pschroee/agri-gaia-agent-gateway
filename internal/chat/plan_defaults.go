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
	derive func(ctx context.Context, read platform.Doer, body json.RawMessage) (string, bool)
}

var derivedDefaults = map[string]derivedDefault{
	platform.AssessSplitPath: {field: "name", derive: splitDefaultName},
}

// splitDefaultName reads the source dataset of a split call and returns the name the handler gives the new one.
func splitDefaultName(ctx context.Context, read platform.Doer, body json.RawMessage) (string, bool) {
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
	value, ok := d.derive(ctx, read, req.Body)
	if !ok {
		return req, false
	}
	return plan.WithDerived(cur, req, resolve, d.field, value)
}

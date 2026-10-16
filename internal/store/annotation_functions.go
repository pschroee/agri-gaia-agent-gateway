// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"time"
)

// States of a pre-annotation function the agent built from a model (table annotation_functions, V2).
// ready is what CVAT answers for a function it lists; the other three come from the gateway's own
// register: deploying until CVAT lists it, failed when the platform's build task failed, missing
// when the function disappeared from CVAT (its catalogue is volatile).
const (
	AnnotationFunctionDeploying = "deploying"
	AnnotationFunctionReady     = "ready"
	AnnotationFunctionFailed    = "failed"
	AnnotationFunctionMissing   = "missing"
)

// AnnotationFunction is a function the agent built from a model of the platform (V2). Name is the
// identifier in CVAT and nuclio and is built by the gateway; the classes stand in the order the
// model outputs them.
type AnnotationFunction struct {
	Name            string    `json:"name"`
	ChatID          string    `json:"chat_id"`
	Username        string    `json:"username"`
	ModelID         int64     `json:"model_id"`
	ModelSHA256     string    `json:"model_sha256"`
	Kind            string    `json:"kind"`
	Labels          []string  `json:"labels"`
	InputSize       int       `json:"input_size"`
	TemplateVersion int       `json:"template_version"`
	PlatformTask    string    `json:"platform_task,omitempty"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
}

const annotationFunctionCols = `name, chat_id::text, username, model_id, model_sha256, kind, labels, input_size, template_version, platform_task, state, created_at`

// labelsText writes the classes of a function for the column.
func labelsText(labels []string) (string, error) {
	if len(labels) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseLabels reads the classes of a function from the column.
func parseLabels(text string) ([]string, error) {
	out := []string{}
	if text == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddAnnotationFunction records a built function. The row keeps the build and its state.
//
// The same model gives the same name, so a function that failed or vanished is built again under the
// name of its row: that row is refreshed (state deploying, the new task) instead of failing on the
// primary key. The function is the same one, only built anew.
func (s *Store) AddAnnotationFunction(ctx context.Context, f AnnotationFunction) (AnnotationFunction, error) {
	if f.State == "" {
		f.State = AnnotationFunctionDeploying
	}
	labels, err := labelsText(f.Labels)
	if err != nil {
		return AnnotationFunction{}, err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO annotation_functions
			(name, chat_id, username, model_id, model_sha256, kind, labels, input_size, template_version, platform_task, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (name) DO UPDATE SET
			chat_id = EXCLUDED.chat_id, username = EXCLUDED.username, model_id = EXCLUDED.model_id,
			model_sha256 = EXCLUDED.model_sha256, kind = EXCLUDED.kind, labels = EXCLUDED.labels,
			input_size = EXCLUDED.input_size, template_version = EXCLUDED.template_version,
			platform_task = EXCLUDED.platform_task, state = EXCLUDED.state, created_at = now()
		RETURNING created_at`,
		f.Name, f.ChatID, f.Username, f.ModelID, f.ModelSHA256, f.Kind, labels, f.InputSize,
		f.TemplateVersion, f.PlatformTask, f.State).Scan(&f.CreatedAt)
	if err != nil {
		return AnnotationFunction{}, err
	}
	return f, nil
}

// AnnotationFunctionsOf lists the functions of one user, oldest first. The functions belong to the
// user (they are visible to every CVAT user), not to one chat, so the listing does not filter by
// chat: the same function may have been built in another chat.
func (s *Store) AnnotationFunctionsOf(ctx context.Context, username string) ([]AnnotationFunction, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+annotationFunctionCols+`
		FROM annotation_functions WHERE username=$1 ORDER BY created_at, name`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnotationFunction{}
	for rows.Next() {
		var f AnnotationFunction
		var labels string
		if err := rows.Scan(&f.Name, &f.ChatID, &f.Username, &f.ModelID, &f.ModelSHA256, &f.Kind, &labels,
			&f.InputSize, &f.TemplateVersion, &f.PlatformTask, &f.State, &f.CreatedAt); err != nil {
			return nil, err
		}
		if f.Labels, err = parseLabels(labels); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetAnnotationFunctionState writes the state of a function. The name is the identifier, so the
// state of a function one chat built can be brought up to date by another chat of the same user.
func (s *Store) SetAnnotationFunctionState(ctx context.Context, name, state string) error {
	_, err := s.pool.Exec(ctx, `UPDATE annotation_functions SET state=$2 WHERE name=$1`, name, state)
	return err
}

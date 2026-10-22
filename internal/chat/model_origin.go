// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// modelFromTrainingPath is the call that creates a model from a training container. It is a GET and creates a new
// model on every call (Backend, routers/train.py).
var modelFromTrainingPath = regexp.MustCompile(`^/train/containers/([0-9]+)/model$`)

// modelOriginReadTimeout bounds the read of the list of training containers after the call.
const modelOriginReadTimeout = 10 * time.Second

// joinNote joins notes of the gateway with a space; empty parts drop out.
func joinNote(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// recordModelFromTraining records where a model came from (R1.6) after a successful call that created it from a
// training container, whichever binding, tool or plan step made the call (issue #211): the origin is the
// gateway's bookkeeping, not a property of one tool. The dataset comes from a read of the gateway (the answer of
// the platform names none). Without a dataset nothing is recorded (P2: an origin without the images the model saw
// would let a check measure on foreign images). It returns the note for the answer, or "".
func (m *Manager) recordModelFromTraining(ctx context.Context, chatID string, req platform.Request, res platform.Result) string {
	if req.Method != http.MethodGet || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return ""
	}
	sub := modelFromTrainingPath.FindStringSubmatch(req.Path)
	if sub == nil {
		return ""
	}
	trainID, err := strconv.ParseInt(sub[1], 10, 64)
	if err != nil {
		return ""
	}
	modelID := platform.ModelIDOf(res)
	if modelID <= 0 {
		slog.Warn("model origin not recorded", "chat", chatID, "training", trainID, "reason", "no model id in the answer")
		return ""
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelOriginReadTimeout)
	defer cancel()
	do := func(c context.Context, r platform.Request) (platform.Result, error) {
		return m.platformDo(c, chatID, r)
	}
	dsID, err := platform.TrainingDatasetID(rctx, do, trainID)
	result := "ok"
	if err != nil || dsID <= 0 {
		result = "no dataset"
	}
	m.LogCallBy(rctx, m.slotOf(chatID), chatID, "platform", "platform",
		fmt.Sprintf("GET /train/containers (dataset of training %d)", trainID), result)
	if err != nil || dsID <= 0 {
		slog.Warn("model origin not recorded", "chat", chatID, "model", modelID, "training", trainID, "dataset", dsID, "error", err)
		return ""
	}
	if err := m.st.RecordModelOrigin(rctx, store.ModelOrigin{
		ModelID: modelID, ChatID: chatID, Source: store.OriginTraining,
		TrainContainerID: trainID, SeenDatasetIDs: []int64{dsID},
	}); err != nil {
		slog.Error("model origin not recorded", "chat", chatID, "model", modelID, "error", err)
		return ""
	}
	return fmt.Sprintf("The gateway recorded where model %d came from (training container %d, dataset %d); %s checks it.",
		modelID, trainID, dsID, platform.ToolRef("assess_model"))
}

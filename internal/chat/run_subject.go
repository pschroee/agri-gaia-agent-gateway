// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// trainConfigDoc is the platform's answer to GET /train/containers/<id>/config (routers/train.py): the settings
// under "config.train", and what the container was built from.
type trainConfigDoc struct {
	Config struct {
		Train struct {
			Schema json.RawMessage            `json:"schema"`
			Values map[string]json.RawMessage `json:"values"`
		} `json:"train"`
	} `json:"config"`
	Provider     string `json:"provider"`
	Architecture struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	} `json:"architecture"`
	DatasetID int64 `json:"dataset_id"`
}

// readTrainConfig reads the configuration of a training container; a non-empty string is the reason it failed.
func readTrainConfig(ctx context.Context, read platform.Doer, id string) (trainConfigDoc, string) {
	var doc trainConfigDoc
	return doc, readJSONInto(ctx, read, "/train/containers/"+id+"/config", &doc)
}

// subjectOf says what a training container trains: its architecture and dataset, in the form of a search's base.
func subjectOf(ctx context.Context, read platform.Doer, doc trainConfigDoc, containerID int64) SearchBase {
	b := SearchBase{ContainerID: containerID, Provider: platform.VisibleControls(doc.Provider),
		Category:     platform.VisibleControls(doc.Architecture.Category),
		Architecture: platform.VisibleControls(doc.Architecture.Name)}
	if doc.DatasetID > 0 {
		n := readNamed(ctx, read, doc.DatasetID)
		b.Dataset = &n
	}
	return b
}

// subjectText names a training run for the overview (issue #227): "<architecture> on <dataset>", the architecture
// or the dataset alone, else the container.
func subjectText(b SearchBase) string {
	ds := ""
	if b.Dataset != nil {
		switch {
		case b.Dataset.Name != "":
			ds = b.Dataset.Name
		case b.Dataset.ID > 0:
			ds = "dataset " + strconv.FormatInt(b.Dataset.ID, 10)
		}
	}
	switch {
	case b.Architecture != "" && ds != "":
		return b.Architecture + " on " + ds
	case b.Architecture != "":
		return b.Architecture
	case ds != "":
		return "Training on " + ds
	case b.ContainerID > 0:
		return "Training container " + strconv.FormatInt(b.ContainerID, 10)
	}
	return "Training run"
}

// declareTrainingEarly makes the trial of a training run when the call that starts it returns, not at the first
// poll of the running container (issue #227): the container is pulled and started by a task first, which can take
// longer than a short training. Searches link the trials they declared themselves, so a container with such a trial
// waiting gets none here. The job is not marked as a trial's (trialJobs), so that the note of the job outside a plan
// stays.
func (m *Manager) declareTrainingEarly(ctx context.Context, chatID, jobID string) {
	if m.st == nil || jobID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	j, err := m.st.GetWatchJob(ctx, chatID, jobID)
	if err != nil {
		return
	}
	cid := watch.TrainingContainerOf(j.Kind, j.Ref)
	if cid == "" {
		return
	}
	if _, err := m.st.TrialByJob(ctx, j.ID); err == nil {
		return
	}
	if t, err := m.st.TrialByRef(ctx, chatID, store.SearchPathTemplate, cid); err == nil {
		if t.State == store.TrialDeclared || (t.State == store.TrialRunning && t.JobID == "") {
			return
		}
	}
	jj := j
	jj.Ref = cid
	if _, ok := m.declareSingleTrial(ctx, jj, store.SearchPathTemplate, watch.Progress{}); !ok {
		slog.Warn("training run not listed early", "job", jobID, "container", cid)
	}
}

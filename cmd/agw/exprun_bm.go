// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

// The two steps of the experiment runner that the benchmark added (BM1): the check of the base
// datasets before a run, and the download of a task's result before the cleanup removes it.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agw/internal/exp"
)

// baseNames lists the base datasets a task uses: those its rules name with base:<name>, or all of them
// when the task carries uses_base.
func (r *expRunner) baseNames(task exp.Task) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	rules, _ := task.Delegation["rules"].([]any)
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		ids, _ := rule["ids"].([]any)
		for _, v := range ids {
			if s, _ := v.(string); strings.HasPrefix(s, exp.BaseRef) {
				add(strings.TrimPrefix(s, exp.BaseRef))
			}
		}
	}
	if task.UsesBase && r.base != nil {
		for _, d := range r.base.Datasets {
			add(d.Name)
		}
	}
	sort.Strings(names)
	return names
}

// checkBase reads every base dataset the task uses and compares name, file count and total size with
// grundbestand.yaml. A deviation stops the plan: a run without protection can change the base, and the
// following runs must not start on a changed base.
func (r *expRunner) checkBase(ctx context.Context, task exp.Task) error {
	for _, name := range r.baseNames(task) {
		want, ok := r.base.Lookup(name)
		if !ok {
			return fmt.Errorf("base dataset %q is not listed in %s", name, exp.BaseFile)
		}
		res, err := r.api.ExpDataset(ctx, want.ID)
		if err != nil {
			return fmt.Errorf("base dataset %s (%d) could not be read: %w; the plan stops", name, want.ID, err)
		}
		if res.Status != "ok" || res.HTTPStatus >= 400 {
			return fmt.Errorf("base dataset %s (%d) could not be read: %s; the plan stops", name, want.ID,
				orDefault(res.Message, fmt.Sprintf("platform responds %d", res.HTTPStatus)))
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
			return fmt.Errorf("base dataset %s (%d): the answer is not JSON: %w; the plan stops", name, want.ID, err)
		}
		if diffs := exp.CompareBase(want, got); len(diffs) > 0 {
			return fmt.Errorf("base dataset %s (%d) changed: %s; restore it with grundbestand.py and start again",
				name, want.ID, strings.Join(diffs, ", "))
		}
	}
	return nil
}

// collectResults downloads, for every collect entry of the task, the dataset of that name which the run's chat
// created, to <collect dir>/<run key>/<name>.zip (the run key begins with the plan id). It is called after
// the check and before the cleanup. What goes wrong is returned as notes for the end reason and written to
// the log; the run stays valid, because a missing result is a finding of the run, not of the environment.
func (r *expRunner) collectResults(ctx context.Context, pr expPlannedRun, chatID string, task exp.Task) []string {
	if len(task.Collect) == 0 {
		return nil
	}
	note := func(format string, a ...any) string {
		line := fmt.Sprintf(format, a...)
		fmt.Fprintf(r.out, "run %s: %s\n", pr.Key, line)
		return line
	}
	objs, err := r.api.ExpObjects(ctx, chatID)
	if err != nil {
		return []string{note("collect: register not readable: %v", err)}
	}
	var datasets []string
	for _, o := range objs {
		if o.Resource == "dataset" {
			datasets = appendUnique(datasets, o.ID)
		}
	}
	var notes []string
	for _, spec := range task.Collect {
		// The newest dataset of that name wins when the agent made several.
		found := ""
		for _, id := range datasets {
			raw, err := (expRunnerCheck{api: r.api, chat: chatID}).Get(ctx, "/datasets/"+id)
			if err != nil {
				continue // removed again by the agent, or not readable
			}
			var d struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &d) == nil && d.Name == spec.Name {
				found = id
			}
		}
		if found == "" {
			notes = append(notes, note("collect: dataset %s not created", spec.Name))
			continue
		}
		dsID, err := strconv.ParseInt(found, 10, 64)
		if err != nil {
			notes = append(notes, note("collect: dataset %s has the identifier %q, which is not a number", spec.Name, found))
			continue
		}
		path, err := r.downloadTo(ctx, pr, chatID, dsID, spec.Name)
		if err != nil {
			notes = append(notes, note("collect: dataset %s (%d): %v", spec.Name, dsID, err))
			continue
		}
		note("collect: dataset %s (%d) saved to %s", spec.Name, dsID, path)
	}
	return notes
}

// downloadTo stores one archive; the file appears under its final name only when it is complete.
func (r *expRunner) downloadTo(ctx context.Context, pr expPlannedRun, chatID string, dataset int64, name string) (string, error) {
	dir := filepath.Join(r.collect, filepath.FromSlash(pr.Key))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, name+".zip")
	tmp, err := os.CreateTemp(dir, name+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := r.api.ExpDownload(ctx, chatID, dataset, tmp); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"strings"
	"testing"
)

func form(kv ...string) map[string][]string {
	f := map[string][]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i]] = append(f[kv[i]], kv[i+1])
	}
	return f
}

const smallSpace = `{"lr":{"type":"float","min":0.0001,"max":0.1,"scale":"log"}}`

func TestParseProposalTemplate(t *testing.T) {
	p, err := ParseProposal(form("title", "Learning rate", "path", "template", "base_container_id", "63",
		"space", smallSpace, "budget", `{"max_runs":3,"target":0.95,"patience":2}`, "note", "start wide"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseContainerID != 63 || p.Objective.Metric != "val_acc" || *p.Budget.MaxRuns != 3 || p.Note != "start wide" || len(p.Space) != 1 {
		t.Errorf("proposal: %+v", p)
	}
}

func TestParseProposalNotebook(t *testing.T) {
	p, err := ParseProposal(form("title", "t", "path", "notebook", "entry", "train.py", "args", "--x=1", "dataset_ids", "3",
		"timeout_minutes", "60", "space", smallSpace, "budget", `{}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Run.Entry != "train.py" || p.Run.TimeoutMinutes != 60 || len(p.Run.DatasetIDs) != 1 || p.BaseContainerID != 0 {
		t.Errorf("run: %+v", p.Run)
	}
}

func TestParseProposalRefuses(t *testing.T) {
	base := func(extra ...string) map[string][]string {
		return form(append([]string{"title", "t", "path", "template", "base_container_id", "63", "space", smallSpace, "budget", `{}`}, extra...)...)
	}
	cases := []struct {
		name string
		f    map[string][]string
		want string
	}{
		{"no title", form("path", "template", "base_container_id", "63", "space", smallSpace), "title"},
		{"unknown field", base("code", "x"), "unknown field"},
		{"two titles", base("title", "u"), "given 2 times"},
		{"bad path", form("title", "t", "path", "cloud", "space", smallSpace), "path must be"},
		{"template without a container", form("title", "t", "path", "template", "space", smallSpace), "base_container_id"},
		{"template with an entry", base("entry", "train.py"), "belongs to the notebook way"},
		{"notebook with a container", form("title", "t", "path", "notebook", "base_container_id", "63", "entry", "a.py", "space", smallSpace), "belongs to the template way"},
		{"notebook without an entry", form("title", "t", "path", "notebook", "space", smallSpace), "entry"},
		{"train metric", base("objective", `{"metric":"train_acc"}`), "not allowed"},
		{"bad space", form("title", "t", "path", "template", "base_container_id", "63", "space", `{"lr":{"type":"x"}}`), "space.lr"},
		{"bad budget", form("title", "t", "path", "template", "base_container_id", "63", "space", smallSpace, "budget", `{"max_runs":0}`), "max_runs"},
		{"long note", base("note", strings.Repeat("x", NoteMaxRunes+1)), "note"},
	}
	for _, c := range cases {
		if _, err := ParseProposal(c.f, 0); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", c.name, err, c.want)
		}
	}
}

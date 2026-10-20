// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"agw/internal/platform"
)

// Paths of a search.
const (
	PathTemplate = "template"
	PathNotebook = "notebook"
)

// Limits of the texts of a proposal.
const (
	MaxTitleRunes = 120
	NoteMaxRunes  = 500
)

// Proposal is a search as the agent proposes it with propose_search: the form fields of the call, read and
// checked. The files of the notebook way are not part of it (they come as uploads and are bound by SHA-256 when
// the proposal is taken); Run carries the rest of the run (entry, arguments, datasets, time limit).
type Proposal struct {
	Title     string
	Objective Objective
	Path      string
	// BaseContainerID is the built training container of the template way; 0 on the notebook way.
	BaseContainerID int64
	// Run is the notebook run the search repeats (entry, args, dataset_ids, timeout_minutes); zero on the template way.
	Run    platform.RunBody
	Space  Space
	Budget Budget
	// Note is the agent's own text, shown as not verified.
	Note string
}

// ParseProposal reads the form of propose_search: title, path ("template" or "notebook"), objective, space and
// budget as JSON texts, base_container_id (template) or entry, args, dataset_ids and timeout_minutes (notebook),
// and note. maxTimeoutMin is the ceiling of a notebook run in this setup (0: the default ceiling). It checks
// the shapes only: that the base container is the chat's, that the keys of the space exist in the template and
// that the budget fits the compute calls left are checked where the proposal is taken.
func ParseProposal(form map[string][]string, maxTimeoutMin int) (Proposal, error) {
	var p Proposal
	for k, vs := range form {
		switch k {
		case "title", "path", "objective", "space", "budget", "base_container_id", "note":
			if len(vs) != 1 {
				return p, fmt.Errorf("field %q given %d times", k, len(vs))
			}
		case "entry", "args", "dataset_ids", "timeout_minutes":
		default:
			return p, fmt.Errorf("unknown field %q", k)
		}
	}
	one := func(k string) string {
		if vs := form[k]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	p.Title = strings.TrimSpace(one("title"))
	if p.Title == "" || !utf8.ValidString(p.Title) || utf8.RuneCountInString(p.Title) > MaxTitleRunes {
		return p, fmt.Errorf("title: between 1 and %d characters", MaxTitleRunes)
	}
	note := strings.TrimSpace(one("note"))
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > NoteMaxRunes {
		return p, fmt.Errorf("note: at most %d characters", NoteMaxRunes)
	}
	p.Note = note
	var err error
	if p.Objective, err = ParseObjective([]byte(one("objective"))); err != nil {
		return p, err
	}
	if p.Space, err = ParseSpace([]byte(one("space"))); err != nil {
		return p, err
	}
	if p.Budget, err = ParseBudget([]byte(one("budget"))); err != nil {
		return p, err
	}
	p.Path = strings.TrimSpace(one("path"))
	switch p.Path {
	case PathTemplate:
		for _, k := range []string{"entry", "args", "dataset_ids", "timeout_minutes"} {
			if len(form[k]) > 0 {
				return p, fmt.Errorf("%s belongs to the notebook way", k)
			}
		}
		id, err := strconv.ParseInt(strings.TrimSpace(one("base_container_id")), 10, 64)
		if err != nil || id <= 0 {
			return p, errors.New("base_container_id: the id of a built training container is required for path template")
		}
		p.BaseContainerID = id
	case PathNotebook:
		if len(form["base_container_id"]) > 0 {
			return p, errors.New("base_container_id belongs to the template way")
		}
		run := map[string][]string{}
		for _, k := range []string{"entry", "args", "dataset_ids", "timeout_minutes"} {
			if len(form[k]) > 0 {
				run[k] = form[k]
			}
		}
		if p.Run, err = platform.ParseRunForm(run, maxTimeoutMin); err != nil {
			return p, err
		}
	default:
		return p, errors.New(`path must be "template" or "notebook"`)
	}
	return p, nil
}

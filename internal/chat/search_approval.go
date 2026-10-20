// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/search"
)

// The structured data of the approvals of the training search (issue #168). Both kinds go into approvals.search,
// the card tells them apart by Kind.
const (
	searchApprovalProposal   = "proposal"    // the approval of a search: space, budget, base or code, test set
	searchApprovalCodeChange = "code_change" // a notebook run of a search whose code differs from the approved one
)

// SearchAutonomy is what the user hands over with a search: how many turns the agent may take without asking.
type SearchAutonomy struct {
	MaxAutoTurns int `json:"max_auto_turns"`
}

// SearchNamed is a platform object with the name the gateway read.
type SearchNamed struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
	// Images and Classes come from the metadata of the dataset; left out when the platform does not say.
	Images  int `json:"images,omitempty"`
	Classes int `json:"classes,omitempty"`
}

// SearchBase is the template way's starting point: the built training container the trials copy.
type SearchBase struct {
	ContainerID  int64        `json:"container_id"`
	Provider     string       `json:"provider,omitempty"`
	Category     string       `json:"category,omitempty"`
	Architecture string       `json:"architecture,omitempty"`
	Dataset      *SearchNamed `json:"dataset,omitempty"`
}

// SearchHoldout says whether the search has a test set the agent never sees: the images a split held back.
type SearchHoldout struct {
	Available   bool         `json:"available"`
	EvalDataset *SearchNamed `json:"eval_dataset,omitempty"`
	Images      int          `json:"images,omitempty"`
	// FinalState: "" (not run yet), "pending" (the search ended with a winner), "started" (the final test runs).
	FinalState string          `json:"final_state,omitempty"`
	CheckID    string          `json:"check_id,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

// SearchApprovalData is what the card of a search proposal shows, written by the gateway from what it read. The
// agent's own note is the only text of the agent in it.
type SearchApprovalData struct {
	Kind      string                     `json:"kind"`
	SearchID  string                     `json:"search_id"`
	Title     string                     `json:"title"`
	Objective search.Objective           `json:"objective"`
	Space     search.Space               `json:"space"`
	Fixed     map[string]json.RawMessage `json:"fixed,omitempty"`
	Budget    search.Budget              `json:"budget"`
	Compute   struct {
		Needed int `json:"needed"`
		// Left is what the chat has now; LeftAfter what is left once the search has used Needed.
		Left      int `json:"left"`
		LeftAfter int `json:"left_after"`
	} `json:"compute"`
	// ExpiresAt is when the approval lapses undecided (the waiting time of the gateway).
	ExpiresAt time.Time       `json:"expires_at"`
	Path      string          `json:"path"`
	Base      *SearchBase     `json:"base,omitempty"`
	Code      json.RawMessage `json:"code,omitempty"` // notebook.ApprovalData of the run: entry, files with SHA-256 and text, datasets, limits
	Holdout   SearchHoldout   `json:"holdout"`
	Autonomy  SearchAutonomy  `json:"autonomy"`
	Note      string          `json:"note,omitempty"`
}

// SearchCodeChange is the data of the question to the user when a trial of a notebook search runs other code than
// the one the search was approved with.
type SearchCodeChange struct {
	Kind            string             `json:"kind"`
	SearchID        string             `json:"search_id"`
	Title           string             `json:"title"`
	Trial           int                `json:"trial"`
	CodeVersionFrom int                `json:"code_version_from"`
	CodeVersionTo   int                `json:"code_version_to"`
	ChangedFiles    []string           `json:"changed_files"`
	Files           []SearchFileChange `json:"files"`
}

// SearchFileChange is one changed file of a code change: the hashes of the approved and the new version and the
// difference. A diff longer than search.MaxDiffLines is not shown: DiffTooLong is set and NewText carries the
// whole new file. A file without text (binary, large) carries the hashes only.
type SearchFileChange struct {
	Name        string `json:"name"`
	OldSHA256   string `json:"old_sha256,omitempty"`
	NewSHA256   string `json:"new_sha256,omitempty"`
	Added       bool   `json:"added,omitempty"`
	Removed     bool   `json:"removed,omitempty"`
	Diff        string `json:"diff,omitempty"`
	DiffTooLong bool   `json:"diff_too_long,omitempty"`
	NewText     string `json:"new_text,omitempty"`
}

// maxAutoTurns is the wake limit of a search: three turns per trial and three more, at most 50.
func maxAutoTurns(maxRuns int) int {
	return min(50, 3*maxRuns+3)
}

// budgetText describes a budget in the words of the approval.
func budgetText(b search.Budget) string {
	var parts []string
	if b.MaxRuns != nil {
		parts = append(parts, fmt.Sprintf("at most %d trials", *b.MaxRuns))
	}
	if b.MaxComputeMinutes != nil {
		parts = append(parts, fmt.Sprintf("at most %d minutes of compute time", *b.MaxComputeMinutes))
	}
	if b.Target != nil {
		parts = append(parts, "stop at "+strconv.FormatFloat(*b.Target, 'g', -1, 64))
	}
	if b.Patience != nil {
		parts = append(parts, fmt.Sprintf("stop after %d trials without improvement", *b.Patience))
	}
	if len(parts) == 0 {
		return "no limit of its own (only the compute calls left in this chat and the time limit)"
	}
	return strings.Join(parts, ", ")
}

// searchSummary is the text of the approval, written from the data: the gateway's own words, the agent's note at
// the end and marked as not verified.
func searchSummary(d SearchApprovalData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Training search %q: the agent runs trial after trial inside this space and budget, chosen by %s (%s).\n",
		d.Title, d.Objective.Metric, d.Objective.Mode)
	b.WriteString("Search space:\n")
	for _, name := range d.Space.Names() {
		fmt.Fprintf(&b, "  %s: %s\n", name, d.Space[name].String())
	}
	if len(d.Fixed) > 0 {
		names := make([]string, 0, len(d.Fixed))
		for k := range d.Fixed {
			names = append(names, k)
		}
		sort.Strings(names)
		b.WriteString("Everything else stays fixed:")
		for _, k := range names {
			fmt.Fprintf(&b, " %s=%s", k, string(d.Fixed[k]))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Budget: %s. Every trial uses one compute call (%d needed, %d left afterwards).\n", budgetText(d.Budget), d.Compute.Needed, d.Compute.LeftAfter)
	switch {
	case d.Base != nil:
		fmt.Fprintf(&b, "Each trial is a copy of training container %d (%s)", d.Base.ContainerID,
			strings.TrimSpace(strings.Join([]string{d.Base.Provider, d.Base.Architecture, d.Base.Category}, " ")))
		if d.Base.Dataset != nil {
			fmt.Fprintf(&b, " on dataset %s", namedText(*d.Base.Dataset))
			if d.Base.Dataset.Images > 0 {
				fmt.Fprintf(&b, ", %d images", d.Base.Dataset.Images)
			}
			if d.Base.Dataset.Classes > 0 {
				fmt.Fprintf(&b, ", %d classes", d.Base.Dataset.Classes)
			}
		}
		b.WriteString(", with other settings; no new image is built.\n")
	case len(d.Code) > 0:
		b.WriteString("Each trial runs the same code in the notebook server; only params.json changes between trials. Another code asks you again.\n")
	}
	if d.Holdout.Available && d.Holdout.EvalDataset != nil {
		fmt.Fprintf(&b, "Test set: %d images of dataset %s were held back; the agent never sees them and tests the winner once at the end.\n",
			d.Holdout.Images, namedText(*d.Holdout.EvalDataset))
	} else {
		b.WriteString("Test set: none. The dataset was not split first, so the winner is judged by its validation value only.\n")
	}
	fmt.Fprintf(&b, "After you approve, the agent works without asking for up to %d turns of its own.", d.Autonomy.MaxAutoTurns)
	if d.Note != "" {
		b.WriteString("\nAgent's note (not verified): " + d.Note)
	}
	return platform.VisibleControls(b.String())
}

func namedText(n SearchNamed) string {
	if n.Name != "" {
		return strconv.Quote(n.Name) + " (#" + strconv.FormatInt(n.ID, 10) + ")"
	}
	return "#" + strconv.FormatInt(n.ID, 10)
}

// readNamed reads the name of a dataset for the approval; a failed read leaves the name empty.
func readNamed(ctx context.Context, read platform.Doer, id int64) SearchNamed {
	n := SearchNamed{ID: id}
	if read == nil || id <= 0 {
		return n
	}
	res, err := read(ctx, platform.Request{Method: "GET", Path: "/datasets/" + strconv.FormatInt(id, 10)})
	if err != nil || res.Status != "ok" || res.HTTPStatus >= 400 {
		return n
	}
	var d struct {
		Name      string   `json:"name"`
		Filecount int      `json:"filecount"`
		Labels    []string `json:"annotation_labels"`
	}
	if json.Unmarshal([]byte(res.Body), &d) == nil {
		n.Name = platform.VisibleControls(d.Name)
		if d.Filecount > 0 {
			n.Images = d.Filecount
		}
		n.Classes = len(d.Labels)
	}
	return n
}

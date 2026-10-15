// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The templates for the summaries of the notebook calls (own code C-E3) live in their own file:
// init appends them, so the switch in summary.go stays what it is. The code of a run is not part of
// the summary — the approval's preview carries it (NotebookPreview); the summary says what happens
// and which objects take part.
func init() { registerDescriber(describeNotebook) }

// describeNotebook answers the notebook paths of its feature; a call it does not know keeps the
// built-in template.
func describeNotebook(c SummaryCall, s *summarizer) (string, []string, bool) {
	if !strings.HasPrefix(c.Path, NotebookPrefix) {
		return "", nil, false
	}
	switch {
	case c.Method == http.MethodPost && c.Path == NotebookPrefix+"/runs":
		return s.notebookRun()
	case c.Method == http.MethodPost && c.Path == NotebookPrefix+"/stop":
		return "Stop your notebook server (files stay)", []string{"The files of earlier runs stay in the server."}, true
	}
	return "", nil, false
}

// notebookRun writes the summary of a run: what it starts, with which arguments, which datasets are
// unpacked first and which files it gets. Every form field the template names is marked, so nothing
// of the call stays hidden.
func (s *summarizer) notebookRun() (string, []string, bool) {
	b, err := ParseRunForm(s.call.Form, NotebookTimeoutMax)
	if err != nil {
		return "", nil, false // a form the runner would refuse: the generic template lists it
	}
	for _, k := range []string{"entry", "args", "dataset_ids", "timeout_minutes"} {
		if _, ok := s.call.Form[k]; ok {
			s.usedForm[k] = true
		}
	}
	head := fmt.Sprintf("Run %s in your notebook server (no internet, no platform access, at most %d min)",
		strconv.Quote(b.Entry), b.TimeoutMinutes)
	var details []string
	if len(b.Args) > 0 {
		details = append(details, "Arguments: "+strings.Join(b.Args, " "))
	}
	for _, id := range b.DatasetIDs {
		sid := strconv.FormatInt(id, 10)
		where := "/home/jovyan/work/datasets/" + sid + "/"
		if name, has := s.datasetName(sid); has {
			details = append(details, fmt.Sprintf("Dataset %s %s is unpacked to %s before the run", sid, strconv.Quote(name), where))
		} else {
			details = append(details, fmt.Sprintf("Dataset %s is unpacked to %s before the run", sid, where))
		}
	}
	details = append(details, "The code runs as shown in the approval above, in full.")
	return head, append(details, s.fileLines()...), true
}

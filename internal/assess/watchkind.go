// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// The observer kind of a check (R0.21, G1.7): the watcher of the autonomy plan asks Poll for the state
// of a check and shows a job line while it runs. Nothing here polls on its own — the agent asks
// assess_status for that, and the check records its end in the store either way.

// KindName is the name of the kind.
const KindName = "assessment"

// CheckRoute is the call that starts a check: method and path pattern for watch.RegisterRoute. R1–R3
// start checks on their own paths (their tools carry the model or the deployment in the path); they
// register the same kind for those in their own files.
func CheckRoute() (string, *regexp.Regexp) {
	return http.MethodPost, regexp.MustCompile(`^` + regexp.QuoteMeta(platform.AssessPrefix) + `/checks$`)
}

// RunIDFrom reads the check id out of the answer of a started check.
func RunIDFrom(_ platform.Request, res platform.Result) (string, bool) {
	var doc struct {
		CheckID string `json:"check_id"`
	}
	if res.Status != "ok" || json.Unmarshal([]byte(res.Body), &doc) != nil || doc.CheckID == "" {
		return "", false
	}
	return doc.CheckID, true
}

// WatchKind is the kind of job that observes one check of this chat.
func (s *Service) WatchKind() watch.Kind {
	return watch.Kind{Name: KindName, Resource: delegation.Assessment, Poll: s.poll}
}

// poll answers the state of a check out of its row: queued and running are "running" for the watcher,
// finished is "done" with the header of the report as its line, failed names the error.
func (s *Service) poll(ctx context.Context, j watch.Job, _ watch.Reader) (watch.Progress, error) {
	row, err := s.St.Assessment(ctx, j.ChatID, j.Ref)
	if err != nil {
		return watch.Progress{}, err
	}
	switch row.State {
	case store.AssessFinished:
		pct := 100.0
		return watch.Progress{State: "done", Percent: &pct, Message: TrimMessage(headerOf(row.Report))}, nil
	case store.AssessFailed:
		msg := row.Error
		if msg == "" {
			msg = "the check failed"
		}
		return watch.Progress{State: "failed", Message: TrimMessage(msg)}, nil
	default:
		return watch.Progress{State: "running", Message: TrimMessage(runningLine(row))}, nil
	}
}

// headerOf reads the header of a stored report; without one it computes it from the report.
func headerOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "the check ended without a report"
	}
	var rep Report
	if json.Unmarshal(raw, &rep) != nil {
		return "the check ended without a report"
	}
	if rep.Header != "" {
		return rep.Header
	}
	return Header(rep)
}

// runningLine is the one line a running check shows: its progress note, or its state.
func runningLine(row store.Assessment) string {
	if len(row.Progress) > 0 {
		var p Progress
		if json.Unmarshal(row.Progress, &p) == nil && p.Note != "" {
			return p.Note
		}
	}
	return "check " + row.Kind + " " + row.Subject
}

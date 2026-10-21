// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"strconv"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// TrialDetail is the answer of GET .../experiments/trials/{trial}: the trial, its epochs and GPU samples and the
// end of the log of its job.
type TrialDetail struct {
	Trial   TrialView        `json:"trial"`
	Epochs  []map[string]any `json:"epochs"`
	GPU     []store.TrialGPU `json:"gpu"`
	LogTail string           `json:"log_tail"`
}

// TrialDetail reads one trial of a chat with its series.
func (m *Manager) TrialDetail(ctx context.Context, chatID, trialID string) (TrialDetail, error) {
	t, err := m.st.GetTrial(ctx, chatID, trialID)
	if err != nil {
		return TrialDetail{}, err
	}
	epochs, gpu, err := m.st.TrialSeries(ctx, t.ID)
	if err != nil {
		return TrialDetail{}, err
	}
	d := TrialDetail{Trial: m.trialView(ctx, t, nil), Epochs: flattenEpochs(epochs), GPU: gpu}
	for i := range d.GPU {
		d.GPU[i].TrialID = ""
	}
	if t.JobID != "" {
		if j, err := m.st.GetWatchJob(ctx, chatID, t.JobID); err == nil {
			d.LogTail = watch.Tail(platform.RedactText(j.LogTail))
		}
	}
	return d, nil
}

// flattenEpochs turns the stored epoch lines into {"epoch","at", <figures by name>...}.
func flattenEpochs(list []store.TrialEpoch) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		row := map[string]any{}
		_ = json.Unmarshal(e.Data, &row)
		row["epoch"], row["at"] = e.Epoch, e.At.UTC().Format(time.RFC3339)
		out = append(out, row)
	}
	return out
}

// SearchExport is the export of a search as JSON: the search, its trials with epochs and the record.
type SearchExport struct {
	Search SearchView          `json:"search"`
	Trials []SearchExportTrial `json:"trials"`
	Events []store.SearchEvent `json:"events"`
}

// SearchExportTrial is a trial with its epochs (GPU samples are not exported).
type SearchExportTrial struct {
	TrialView
	// Series are the lines of the epochs as the run wrote them (Epochs of the trial is their number).
	Series []map[string]any `json:"series"`
}

// ExportSearch collects a search for the export.
func (m *Manager) ExportSearch(ctx context.Context, chatID, searchID string) (SearchExport, error) {
	s, err := m.st.GetSearch(ctx, chatID, searchID)
	if err != nil {
		return SearchExport{}, err
	}
	v, _ := m.searchView(ctx, s)
	out := SearchExport{Search: v, Trials: []SearchExportTrial{}}
	all, err := m.Trials(ctx, chatID)
	if err != nil {
		return out, err
	}
	for _, t := range all {
		if t.SearchID != searchID {
			continue
		}
		epochs, _, err := m.st.TrialSeries(ctx, t.ID)
		if err != nil {
			return out, err
		}
		out.Trials = append(out.Trials, SearchExportTrial{TrialView: t, Series: flattenEpochs(epochs)})
	}
	if out.Events, err = m.st.SearchEvents(ctx, searchID); err != nil {
		return out, err
	}
	return out, nil
}

// SearchCSV is the table of the trials of a search as CSV.
func (e SearchExport) CSV() ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"trial", "state", "params", "note", "score", "score_epoch", "epochs", "duration_s", "started_at", "ended_at", "stopped_by", "error", "winner"})
	f := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'g', -1, 64)
	}
	n := func(p *int) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(*p)
	}
	ts := func(p *time.Time) string {
		if p == nil {
			return ""
		}
		return p.UTC().Format(time.RFC3339)
	}
	for _, t := range e.Trials {
		winner := ""
		if e.Search.WinnerIdx != nil && *e.Search.WinnerIdx == t.Idx {
			winner = "yes"
		}
		row := []string{strconv.Itoa(t.Idx), t.State, string(t.Params), t.Note, f(t.Score), n(t.ScoreEpoch), n(t.Trial.Epochs),
			strconv.FormatFloat(t.DurationS, 'f', 1, 64), ts(t.StartedAt), ts(t.EndedAt), t.StoppedBy, t.Error, winner}
		for i, c := range row { // a cell that starts like a formula is not one in a spreadsheet
			if c != "" && (c[0] == '=' || c[0] == '+' || c[0] == '-' || c[0] == '@') && i != 0 && i != 4 && i != 5 && i != 6 && i != 7 {
				row[i] = "'" + c
			}
		}
		_ = w.Write(row)
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

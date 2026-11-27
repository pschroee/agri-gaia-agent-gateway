// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"io"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

// Issue #281: pi's automatic compaction per plan. The helpers live in their own file so that the runner's main
// file changes by a few lines only.

// compactionProfile is the plan's compaction setting for the experiment profile of a chat; nil when the plan
// does not compact (the default of experiments, X2.5).
func compactionProfile(p *exp.Plan) *agwclient.ExpCompaction {
	if p == nil || !p.Compaction.Auto() {
		return nil
	}
	return &agwclient.ExpCompaction{Mode: exp.CompactionAuto, ReserveTokens: p.Compaction.ReserveTokens}
}

// catalogueWindows maps each model ID of the gateway's catalogue to its context window.
func catalogueWindows(models []agwclient.Model) map[string]int64 {
	out := make(map[string]int64, len(models))
	for _, m := range models {
		out[m.ID] = m.ContextWindow
	}
	return out
}

// compactionLog writes one line per compaction of a run to the run log: the gateway stores every compaction as a
// message with the role "compaction" (reason, tokens before, estimated tokens after, usage of the summary).
type compactionLog struct {
	w    io.Writer
	run  string
	seen map[int64]bool
}

func newCompactionLog(w io.Writer, run string) *compactionLog {
	return &compactionLog{w: w, run: run, seen: map[int64]bool{}}
}

// observe reports the compactions of the chat that it has not reported yet.
func (l *compactionLog) observe(det agwclient.ChatDetail) {
	for _, m := range det.Messages {
		if m.Role != "compaction" || l.seen[m.Seq] {
			continue
		}
		l.seen[m.Seq] = true
		var c struct {
			Reason       string `json:"reason"`
			TokensBefore int64  `json:"tokensBefore"`
			TokensAfter  int64  `json:"estimatedTokensAfter"`
			Usage        struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(m.Message, &c)
		fmt.Fprintf(l.w, "run %s: context compacted (%s): %d tokens before, about %d after; summary call %d in, %d out tokens; %d so far\n",
			l.run, c.Reason, c.TokensBefore, c.TokensAfter, c.Usage.Input, c.Usage.Output, len(l.seen))
	}
}

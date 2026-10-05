package main

import (
	"fmt"
	"sort"
	"strings"

	"agw/internal/agwclient"
)

// agw chat execs: tool executions of the orchestrator, reconciled with the calls
// requested at the LLM proxy (E9).
func (a *app) cmdChatExecs(args []string) error {
	fs := a.flags("chat execs")
	flagged := fs.Bool("flagged", false, "only suspicious calls (not executed, not requested, mismatching)")
	pos, err := a.parse(fs, args, 1, 1, "agw chat execs <id> [--flagged] [--json]")
	if err != nil {
		return err
	}
	r, err := a.c.ToolExecutions(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(r)
	}
	firstArgs := map[string]map[string]any{}
	for _, e := range r.Executions {
		if _, ok := firstArgs[e.ToolCallID]; !ok {
			firstArgs[e.ToolCallID] = e.Args
		}
	}
	var calls []agwclient.ReconciledCall
	for _, c := range r.Calls {
		if c.State == "internal" || (*flagged && !isFlagged(c.State)) {
			continue
		}
		calls = append(calls, c)
	}
	// Suspicious ones first, otherwise in chronological order.
	sort.SliceStable(calls, func(i, j int) bool { return isFlagged(calls[i].State) && !isFlagged(calls[j].State) })
	if len(calls) == 0 {
		if *flagged {
			fmt.Fprintln(a.stdout, "Nothing suspicious.")
		} else {
			fmt.Fprintln(a.stdout, "No tool executions recorded.")
		}
	} else {
		tw := a.table()
		fmt.Fprintln(tw, "Time\tSession\tTool\tOperations\tEvidence\tResult\tDuration\tCommand/path\tID")
		for _, c := range calls {
			at := c.StartedAt
			if at == "" {
				at = c.RequestedAt
			}
			tool := c.Tool
			if c.ExecutedTool != "" && c.ExecutedTool != c.Tool {
				tool += " → " + c.ExecutedTool
			}
			res := "–"
			switch {
			case c.Error != "":
				res = c.Error
			case c.Reason != "":
				res = "according to the session: " + c.Reason
			case c.ExitCode != nil:
				res = fmt.Sprintf("exit %d", *c.ExitCode)
			case c.Executed:
				res = "ok"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", orDefault(fmtClock(at), "–"), sessionName(c.Session), tool,
				orDefault(strings.Join(c.Ops, ", "), "–"), evidenceName(c.State), clipCell(res, 40),
				fmt.Sprintf("%s s", fmtNum(float64(c.DurationMs)/1000, 1, 2)), clipCell(argSummary(firstArgs[c.ToolCallID]), 50), c.ToolCallID)
		}
		tw.Flush()
	}
	s := r.Summary
	bad := s["unexecuted"] + s["unrequested"] + s["mismatch"]
	fmt.Fprintf(a.stdout, "%d confirmed · %d suspicious (%d not executed, %d not requested, %d mismatching) · %d reply aborted · %d refused by pi · %d operations\n",
		s["confirmed"], bad, s["unexecuted"], s["unrequested"], s["mismatch"], s["aborted"], s["rejected"], len(r.Executions))
	return nil
}

// isFlagged: only states that can indicate a bypass are suspicious (M1).
func isFlagged(state string) bool {
	return state == "unexecuted" || state == "unrequested" || state == "mismatch"
}

func evidenceName(state string) string {
	switch state {
	case "confirmed":
		return "confirmed"
	case "unexecuted":
		return "NOT EXECUTED"
	case "unrequested":
		return "NOT REQUESTED"
	case "mismatch":
		return "MISMATCH"
	case "aborted":
		return "reply aborted"
	case "rejected":
		return "refused by pi"
	}
	return state
}

func sessionName(s string) string {
	switch s {
	case "":
		return "–"
	case "main":
		return "main agent"
	}
	run, n, _ := strings.Cut(s, "#")
	if len(run) > 8 {
		run = run[:8]
	}
	if n != "" {
		return "Subagent " + run + " #" + n
	}
	return "Subagent " + run
}

func argSummary(a map[string]any) string {
	if c, ok := a["command"].(string); ok {
		return strings.ReplaceAll(c, "\n", " ⏎ ")
	}
	if p, ok := a["path"].(string); ok {
		return p
	}
	return ""
}

func clipCell(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

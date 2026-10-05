package main

import (
	"fmt"
	"sort"
	"strings"

	"agw/internal/agwclient"
)

// agw chat execs: Werkzeugausführungen des Orchestrators, abgeglichen mit den am
// LLM-Proxy angeforderten Aufrufen (E9).
func (a *app) cmdChatExecs(args []string) error {
	fs := a.flags("chat execs")
	flagged := fs.Bool("flagged", false, "nur auffällige Aufrufe (nicht ausgeführt, nicht angefordert, abweichend)")
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
	// Auffälliges zuerst, sonst in zeitlicher Reihenfolge.
	sort.SliceStable(calls, func(i, j int) bool { return isFlagged(calls[i].State) && !isFlagged(calls[j].State) })
	if len(calls) == 0 {
		if *flagged {
			fmt.Fprintln(a.stdout, "Nichts Auffälliges.")
		} else {
			fmt.Fprintln(a.stdout, "Keine Werkzeugausführungen erfasst.")
		}
	} else {
		tw := a.table()
		fmt.Fprintln(tw, "Zeit\tSitzung\tWerkzeug\tOperationen\tBeleg\tErgebnis\tDauer\tBefehl/Pfad\tKennung")
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
				res = "laut Sitzung: " + c.Reason
			case c.ExitCode != nil:
				res = fmt.Sprintf("Exit %d", *c.ExitCode)
			case c.Executed:
				res = "ok"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", orDefault(fmtClock(at), "–"), sessionName(c.Session), tool,
				orDefault(strings.Join(c.Ops, ", "), "–"), evidenceName(c.State), clipCell(res, 40),
				fmt.Sprintf("%s s", deNum(float64(c.DurationMs)/1000, 1, 2)), clipCell(argSummary(firstArgs[c.ToolCallID]), 50), c.ToolCallID)
		}
		tw.Flush()
	}
	s := r.Summary
	bad := s["unexecuted"] + s["unrequested"] + s["mismatch"]
	fmt.Fprintf(a.stdout, "%d belegt · %d auffällig (%d nicht ausgeführt, %d nicht angefordert, %d abweichend) · %d Antwort abgebrochen · %d von pi abgewiesen · %d Operationen\n",
		s["confirmed"], bad, s["unexecuted"], s["unrequested"], s["mismatch"], s["aborted"], s["rejected"], len(r.Executions))
	return nil
}

// isFlagged: auffällig sind nur Zustände, die auf eine Umgehung deuten können (M1).
func isFlagged(state string) bool {
	return state == "unexecuted" || state == "unrequested" || state == "mismatch"
}

func evidenceName(state string) string {
	switch state {
	case "confirmed":
		return "belegt"
	case "unexecuted":
		return "NICHT AUSGEFÜHRT"
	case "unrequested":
		return "NICHT ANGEFORDERT"
	case "mismatch":
		return "ABWEICHEND"
	case "aborted":
		return "Antwort abgebrochen"
	case "rejected":
		return "von pi abgewiesen"
	}
	return state
}

func sessionName(s string) string {
	switch s {
	case "":
		return "–"
	case "main":
		return "Hauptagent"
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

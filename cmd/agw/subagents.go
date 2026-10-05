package main

import (
	"fmt"

	"agw/internal/agwclient"
)

// ---- Subagenten und Modellaufrufe -----------------------------------------------------------

func (a *app) cmdChatSubagents(args []string) error {
	fs := a.flags("chat subagents")
	usage := "agw chat subagents <id> [N] [--json]"
	pos, err := a.parse(fs, args, 1, 2, usage)
	if err != nil {
		return err
	}
	id := pos[0]
	if len(pos) == 2 {
		n, err := parseCount(pos[1])
		if err != nil {
			return usagef("%v – Aufruf: %s", err, usage)
		}
		c, err := a.c.SetMaxSubagents(a.ctx, id, n)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(c)
		}
		fmt.Fprintf(a.stdout, "Grenze für Subagenten in Chat %s: %d (bisher gestartet %d; wirkt sofort).\n", c.ID, c.MaxSubagents, c.Subagents)
		return nil
	}
	det, err := a.c.Chat(a.ctx, id)
	if err != nil {
		return err
	}
	entries := det.SubagentEntries
	if entries == nil {
		entries = []agwclient.SubagentEntry{}
	}
	if a.json {
		return a.printJSON(map[string]any{"max_subagents": det.Chat.MaxSubagents, "subagents": det.Chat.Subagents, "entries": entries})
	}
	w := a.stdout
	fmt.Fprintf(w, "Subagenten: Grenze %d, gestartet %d\n", det.Chat.MaxSubagents, det.Chat.Subagents)
	if len(entries) == 0 {
		fmt.Fprintln(w, "Noch keine Subagenten-Läufe.")
		return nil
	}
	for _, r := range groupRuns(entries) {
		fmt.Fprintf(w, "\nLauf %s · Agent %s\n", r.id, orDefault(r.agent, "(unbekannt)"))
		// Ergebnisse den Aufrufen der Reihe nach je Werkzeugname zuordnen.
		pending := map[string][]int{}
		type call struct {
			e      agwclient.SubagentEntry
			result *agwclient.SubagentEntry
		}
		var lines []any
		for _, e := range r.entries {
			switch e.Kind {
			case "tool_call":
				lines = append(lines, &call{e: e})
				pending[e.Payload.Name] = append(pending[e.Payload.Name], len(lines)-1)
			case "tool_result":
				if q := pending[e.Payload.Name]; len(q) > 0 {
					e := e
					lines[q[0]].(*call).result = &e
					pending[e.Payload.Name] = q[1:]
					continue
				}
				lines = append(lines, e)
			default:
				lines = append(lines, e)
			}
		}
		for _, l := range lines {
			switch v := l.(type) {
			case *call:
				line := "  ▶ " + v.e.Payload.Name
				if args := compactJSON([]byte(v.e.Payload.Arguments), 160); args != "" {
					line += " " + args
				}
				fmt.Fprintf(w, "%s  [%s]\n", line, confirmedLabel(v.e.Confirmed))
				if v.result != nil {
					fmt.Fprintf(w, "    %s %s\n", resultMark(v.result.Payload.IsError), a.dim(firstLine(v.result.Payload.Text, 160)))
				} else {
					fmt.Fprintf(w, "    %s\n", a.dim("(noch kein Ergebnis)"))
				}
			case agwclient.SubagentEntry:
				switch v.Kind {
				case "task":
					fmt.Fprintf(w, "  Auftrag: %s\n", truncate(oneLine(v.Payload.Text), 100))
				case "text":
					fmt.Fprintf(w, "  Text: %s  [%s]\n", truncate(oneLine(v.Payload.Text), 140), confirmedLabel(v.Confirmed))
				case "tool_result":
					fmt.Fprintf(w, "  %s %s: %s\n", resultMark(v.Payload.IsError), v.Payload.Name, a.dim(firstLine(v.Payload.Text, 160)))
				}
			}
		}
	}
	fmt.Fprintln(w, a.dim("\nQuelle ist die Sitzungsdatei in der Sandbox. „belegt“: die Antwort ist am LLM-Proxy erfasst; „nur Sandbox“: nicht gegengeprüft."))
	return nil
}

func confirmedLabel(ok bool) string {
	if ok {
		return "belegt"
	}
	return "nur Sandbox"
}

type subRun struct {
	id, agent string
	entries   []agwclient.SubagentEntry
}

// groupRuns fasst die Einträge je Lauf zusammen, in der Reihenfolge des ersten Auftretens.
func groupRuns(es []agwclient.SubagentEntry) []*subRun {
	var out []*subRun
	idx := map[string]*subRun{}
	for _, e := range es {
		r := idx[e.RunID]
		if r == nil {
			r = &subRun{id: e.RunID}
			idx[e.RunID] = r
			out = append(out, r)
		}
		if r.agent == "" {
			r.agent = e.Agent
		}
		r.entries = append(r.entries, e)
	}
	return out
}

func (a *app) cmdChatCalls(args []string) error {
	fs := a.flags("chat calls")
	pos, err := a.parse(fs, args, 1, 1, "agw chat calls <id> [--json]")
	if err != nil {
		return err
	}
	calls, err := a.c.LLMCalls(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		if calls == nil {
			calls = []agwclient.LLMCall{}
		}
		return a.printJSON(calls)
	}
	if len(calls) == 0 {
		fmt.Fprintln(a.stdout, "Keine Modellaufrufe erfasst.")
		return nil
	}
	// Tarif nur nennen, wenn das Modell einen hat; ohne Tarif wäre „Nebentarif“ irreführend.
	hasTariff := map[string]bool{}
	if ms, err := a.c.Models(a.ctx); err == nil {
		for _, m := range ms {
			t := m.Tariff != nil && len(m.Tariff.PeakWindowsUTC) > 0
			hasTariff[m.ID], hasTariff[m.Model] = t, t
		}
	}
	tariff := func(c agwclient.LLMCall) string {
		if t, known := hasTariff[c.Model]; known && !t {
			return "–"
		}
		return tariffLabel(c.Peak)
	}
	tw := a.table()
	fmt.Fprintln(tw, "Zeit\tArt\tein\taus\tCache\tKosten\tTarif\tWerkzeuge\tStatus\tDauer")
	var sum, sumSub float64
	var in, out, cache int64
	var sub int
	for _, c := range calls {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s s\n", fmtClock(c.StartedAt), mainLabel(c.Main),
			fmtInt(c.Input), fmtInt(c.Output), fmtInt(c.CacheRead), fmtCost(c.Cost), tariff(c),
			orDefault(llmToolNames(c), "–"), fmtStatus(c.Status), deNum(float64(c.DurationMs)/1000, 1, 1))
		sum += c.Cost
		in, out, cache = in+c.Input, out+c.Output, cache+c.CacheRead
		if !c.Main {
			sub++
			sumSub += c.Cost
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Summe: %d Modellaufrufe · ein %s, aus %s, Cache %s · %s (davon Sub/sonstige %d Aufrufe, %s)\n",
		len(calls), fmtInt(in), fmtInt(out), fmtInt(cache), fmtCost(sum), sub, fmtCost(sumSub))
	fmt.Fprintln(a.stdout, a.dim("Gemessen am LLM-Proxy außerhalb der Sandbox. Haupt: Antwort der Hauptsitzung; Sub: Subagenten, Kompaktierung u. Ä."))
	return nil
}

package main

import (
	"fmt"

	"agw/internal/agwclient"
)

// ---- subagents and model calls --------------------------------------------------------------

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
			return usagef("%v – usage: %s", err, usage)
		}
		c, err := a.c.SetMaxSubagents(a.ctx, id, n)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(c)
		}
		fmt.Fprintf(a.stdout, "Subagent limit in chat %s: %d (started so far %d; takes effect immediately).\n", c.ID, c.MaxSubagents, c.Subagents)
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
	fmt.Fprintf(w, "Subagents: limit %d, started %d\n", det.Chat.MaxSubagents, det.Chat.Subagents)
	if len(entries) == 0 {
		fmt.Fprintln(w, "No subagent runs yet.")
		return nil
	}
	for _, r := range groupRuns(entries) {
		fmt.Fprintf(w, "\nrun %s · agent %s\n", r.id, orDefault(r.agent, "(unknown)"))
		// Match results to the calls in order, per tool name.
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
					fmt.Fprintf(w, "    %s\n", a.dim("(no result yet)"))
				}
			case agwclient.SubagentEntry:
				switch v.Kind {
				case "task":
					fmt.Fprintf(w, "  task: %s\n", truncate(oneLine(v.Payload.Text), 100))
				case "text":
					fmt.Fprintf(w, "  text: %s  [%s]\n", truncate(oneLine(v.Payload.Text), 140), confirmedLabel(v.Confirmed))
				case "tool_result":
					fmt.Fprintf(w, "  %s %s: %s\n", resultMark(v.Payload.IsError), v.Payload.Name, a.dim(firstLine(v.Payload.Text, 160)))
				}
			}
		}
	}
	fmt.Fprintln(w, a.dim("\nThe source is the session file in the sandbox. \"confirmed\": the reply was recorded at the LLM proxy; \"sandbox only\": not cross-checked."))
	return nil
}

func confirmedLabel(ok bool) string {
	if ok {
		return "confirmed"
	}
	return "sandbox only"
}

type subRun struct {
	id, agent string
	entries   []agwclient.SubagentEntry
}

// groupRuns groups the entries per run, in order of first appearance.
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
		fmt.Fprintln(a.stdout, "No model calls recorded.")
		return nil
	}
	// Name the tariff only if the model has one; without a tariff "off-peak tariff" would be misleading.
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
	fmt.Fprintln(tw, "Time\tKind\tin\tout\tCache\tCost\tTariff\tTools\tStatus\tDuration")
	var sum, sumSub float64
	var in, out, cache int64
	var sub int
	for _, c := range calls {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s s\n", fmtClock(c.StartedAt), mainLabel(c.Main),
			fmtInt(c.Input), fmtInt(c.Output), fmtInt(c.CacheRead), fmtCost(c.Cost), tariff(c),
			orDefault(llmToolNames(c), "–"), fmtStatus(c.Status), fmtNum(float64(c.DurationMs)/1000, 1, 1))
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
	fmt.Fprintf(a.stdout, "Total: %d model calls · in %s, out %s, cache %s · %s (of which sub/other %d calls, %s)\n",
		len(calls), fmtInt(in), fmtInt(out), fmtInt(cache), fmtCost(sum), sub, fmtCost(sumSub))
	fmt.Fprintln(a.stdout, a.dim("Measured at the LLM proxy outside the sandbox. main: reply of the main session; sub: subagents, compaction and the like."))
	return nil
}

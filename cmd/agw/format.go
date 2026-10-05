package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agw/internal/agwclient"
)

// fmtNum formats a number with a decimal point: at least minDec, at most maxDec digits.
func fmtNum(v float64, minDec, maxDec int) string {
	s := strconv.FormatFloat(v, 'f', maxDec, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		keep := i + 1 + minDec
		for len(s) > keep && s[len(s)-1] == '0' {
			s = s[:len(s)-1]
		}
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

func fmtPrice(v float64) string { return fmtNum(v, 2, 4) }

func fmtCost(v float64) string { return fmtNum(v, 4, 4) + " USD" }

func fmtTokens(t agwclient.Tokens) string {
	return fmt.Sprintf("%d (in %d, out %d, cache %d)", t.Total, t.Input, t.Output, t.CacheRead)
}

func fmtTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

// fmtSince gives the duration since the point in time in short form ("45s", "3m", "2h05m").
func fmtSince(s string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || s == "" {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func chatState(s string) string {
	switch s {
	case "active":
		return "active"
	case "dormant":
		return "dormant"
	}
	return s
}

func slotState(s string) string {
	switch s {
	case "starting":
		return "starting"
	case "idle":
		return "free"
	case "assigned":
		return "assigned"
	case "stopping":
		return "stopping"
	}
	return s
}

func approvalState(s string) string {
	switch s {
	case "pending":
		return "pending"
	case "approved":
		return "approved"
	case "rejected":
		return "rejected"
	case "expired":
		return "expired"
	}
	return s
}

func kindLabel(s string) string {
	switch s {
	case "input":
		return "input"
	case "output":
		return "output"
	}
	return s
}

func activityLabel(a *agwclient.Activity) string {
	if a == nil {
		return ""
	}
	switch a.Kind {
	case "idle":
		return "Waiting"
	case "thinking":
		return "Thinking"
	case "writing":
		return "Writing"
	case "tool":
		if a.Tool != "" {
			return "Running " + a.Tool
		}
		return "Running a tool"
	case "waiting_approval":
		return "Waiting for approval"
	case "starting":
		return "Starting"
	case "preparing":
		if a.Tool != "" {
			return "Preparing " + a.Tool
		}
		return "Preparing a tool call"
	case "compacting":
		return "Summarising the context"
	}
	return a.Kind
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// fmtInt inserts thousands separators ("983,616").
func fmtInt(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtContext describes context usage, automation and number of compactions in one line.
func fmtContext(c agwclient.Chat) string {
	var sb strings.Builder
	cu := c.Context
	switch {
	case cu == nil:
		sb.WriteString("context not measured yet")
	case cu.Tokens == nil || cu.Percent == nil:
		fmt.Fprintf(&sb, "context ? / %s tokens (measured again after the next reply)", fmtInt(cu.Window))
	default:
		fmt.Fprintf(&sb, "context %s / %s tokens (%s %%)", fmtInt(*cu.Tokens), fmtInt(cu.Window), fmtNum(*cu.Percent, 0, 1))
	}
	sb.WriteString(", auto-compaction " + onOff(c.AutoCompact))
	if c.AutoCompact && cu != nil && cu.ThresholdTokens > 0 {
		sb.WriteString(" from " + fmtInt(cu.ThresholdTokens))
	}
	switch {
	case c.Compactions == 1:
		sb.WriteString(", 1 compaction")
	case c.Compactions > 1:
		fmt.Fprintf(&sb, ", %d compactions", c.Compactions)
	}
	return sb.String()
}

// fmtWorkspace: "workspace saved: 1.2 MB, 14 files (2026-09-29 17:05)".
func fmtWorkspace(w *agwclient.Workspace) string {
	var sb strings.Builder
	if w == nil || w.SavedAt == "" {
		sb.WriteString("workspace not saved yet")
	} else {
		files := fmt.Sprintf("%d files", w.Files)
		if w.Files == 1 {
			files = "1 file"
		}
		fmt.Fprintf(&sb, "workspace saved: %s MB, %s (%s)", fmtNum(float64(w.Size)/(1<<20), 1, 1), files, fmtTime(w.SavedAt))
	}
	if w != nil && w.SkippedReason != "" {
		fmt.Fprintf(&sb, "; last time NOT saved (%s): %s", fmtTime(w.SkippedAt), w.SkippedReason)
	}
	return sb.String()
}

func compactionReason(r string) string {
	switch r {
	case "manual":
		return "manual"
	case "threshold":
		return "threshold reached"
	case "overflow":
		return "context overflowed"
	}
	return r
}

// fmtCompacted: "context summarised: 42.000 → approx. 3.100 tokens".
func fmtCompacted(reason string, before, after int64) string {
	head := "context summarised"
	if reason != "" {
		head += " (" + compactionReason(reason) + ")"
	}
	switch {
	case before > 0 && after > 0:
		return fmt.Sprintf("%s: %s → approx. %s tokens", head, fmtInt(before), fmtInt(after))
	case before > 0:
		return fmt.Sprintf("%s: before %s tokens", head, fmtInt(before))
	}
	return head
}

func commandSource(s string) string {
	switch s {
	case "builtin":
		return "built-in"
	case "extension":
		return "extension"
	case "prompt":
		return "prompt"
	case "skill":
		return "skill"
	}
	return s
}

func tariffLabel(peak bool) string {
	if peak {
		return "peak tariff"
	}
	return "off-peak tariff"
}

var dayLabels = map[string]string{"mon": "Mon", "tue": "Tue", "wed": "Wed", "thu": "Thu", "fri": "Fri", "sat": "Sat", "sun": "Sun"}

// fmtDays translates "mon-fri" → "Mon–Fri", "sat,sun" → "Sat, Sun", "all" → "daily".
func fmtDays(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" || d == "all" {
		return "daily"
	}
	parts := strings.Split(d, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if a, b, ok := strings.Cut(p, "-"); ok {
			parts[i] = orDefault(dayLabels[a], a) + "–" + orDefault(dayLabels[b], b)
		} else {
			parts[i] = orDefault(dayLabels[p], p)
		}
	}
	return strings.Join(parts, ", ")
}

// fmtWindows: "Mon–Fri 01:00–09:00 UTC; Sat 02:00–04:00 UTC".
func fmtWindows(ws []agwclient.TariffWindow) string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%s %s–%s UTC", fmtDays(w.Days), w.From, w.To))
	}
	return strings.Join(out, "; ")
}

// oneLine collapses whitespace including line breaks into single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstLine: first line truncated, plus the number of further lines.
func firstLine(s string, max int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := truncate(lines[0], max)
	if len(lines) > 1 {
		out += fmt.Sprintf(" … (+%d lines)", len(lines)-1)
	}
	return out
}

func resultMark(isError bool) string {
	if isError {
		return "✗"
	}
	return "✓"
}

func mainLabel(main bool) string {
	if main {
		return "main"
	}
	return "sub"
}

// fmtStatus: 200 → "ok", 429 → "429 refused", otherwise the number.
func fmtStatus(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "ok"
	case code == 429:
		return "429 refused"
	case code == 0:
		return "–"
	}
	return strconv.Itoa(code)
}

func llmToolNames(c agwclient.LLMCall) string {
	names := make([]string, 0, len(c.ToolCalls))
	for _, t := range c.ToolCalls {
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}

// fmtLLMCallLine: "model call sub · in 500, out 100, cache 0 · 0.0012 USD · tools bash · ok".
func fmtLLMCallLine(c agwclient.LLMCall) string {
	s := fmt.Sprintf("model call %s · in %s, out %s, cache %s · %s", mainLabel(c.Main),
		fmtInt(c.Input), fmtInt(c.Output), fmtInt(c.CacheRead), fmtCost(c.Cost))
	if t := llmToolNames(c); t != "" {
		s += " · tools " + t
	}
	return s + " · " + fmtStatus(c.Status)
}

// fmtClock: time of day (local) of an RFC 3339 timestamp.
func fmtClock(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("15:04:05")
}

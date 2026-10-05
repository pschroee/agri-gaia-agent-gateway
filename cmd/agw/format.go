package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agw/internal/agwclient"
)

// deNum formatiert eine Zahl mit Dezimalkomma: mindestens minDec, höchstens maxDec Stellen.
func deNum(v float64, minDec, maxDec int) string {
	s := strconv.FormatFloat(v, 'f', maxDec, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		keep := i + 1 + minDec
		for len(s) > keep && s[len(s)-1] == '0' {
			s = s[:len(s)-1]
		}
		s = strings.TrimSuffix(s, ".")
	}
	return strings.Replace(s, ".", ",", 1)
}

func fmtPrice(v float64) string { return deNum(v, 2, 4) }

func fmtCost(v float64) string { return deNum(v, 4, 4) + " USD" }

func fmtTokens(t agwclient.Tokens) string {
	return fmt.Sprintf("%d (ein %d, aus %d, Cache %d)", t.Total, t.Input, t.Output, t.CacheRead)
}

func fmtTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

// fmtSince gibt die Dauer seit dem Zeitpunkt kurz an („45s", „3m", „2h05m").
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
		return "ja"
	}
	return "nein"
}

func onOff(b bool) string {
	if b {
		return "an"
	}
	return "aus"
}

func chatState(s string) string {
	switch s {
	case "active":
		return "aktiv"
	case "dormant":
		return "ruhend"
	}
	return s
}

func slotState(s string) string {
	switch s {
	case "starting":
		return "startet"
	case "idle":
		return "frei"
	case "assigned":
		return "vergeben"
	case "stopping":
		return "stoppt"
	}
	return s
}

func approvalState(s string) string {
	switch s {
	case "pending":
		return "offen"
	case "approved":
		return "bestätigt"
	case "rejected":
		return "abgelehnt"
	case "expired":
		return "abgelaufen"
	}
	return s
}

func kindLabel(s string) string {
	switch s {
	case "input":
		return "Eingabe"
	case "output":
		return "Ausgabe"
	}
	return s
}

func activityLabel(a *agwclient.Activity) string {
	if a == nil {
		return ""
	}
	switch a.Kind {
	case "idle":
		return "Wartet"
	case "thinking":
		return "Denkt"
	case "writing":
		return "Schreibt"
	case "tool":
		if a.Tool != "" {
			return "Führt " + a.Tool + " aus"
		}
		return "Führt ein Werkzeug aus"
	case "waiting_approval":
		return "Wartet auf Bestätigung"
	case "starting":
		return "Startet"
	case "preparing":
		if a.Tool != "" {
			return "Bereitet " + a.Tool + " vor"
		}
		return "Bereitet einen Werkzeugaufruf vor"
	case "compacting":
		return "Fasst den Kontext zusammen"
	}
	return a.Kind
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// fmtInt setzt Tausenderpunkte („983.616").
func fmtInt(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtContext beschreibt Kontextauslastung, Automatik und Zahl der Kompaktierungen in einer Zeile.
func fmtContext(c agwclient.Chat) string {
	var sb strings.Builder
	cu := c.Context
	switch {
	case cu == nil:
		sb.WriteString("Kontext noch nicht gemessen")
	case cu.Tokens == nil || cu.Percent == nil:
		fmt.Fprintf(&sb, "Kontext ? / %s Tokens (neu gemessen nach der nächsten Antwort)", fmtInt(cu.Window))
	default:
		fmt.Fprintf(&sb, "Kontext %s / %s Tokens (%s %%)", fmtInt(*cu.Tokens), fmtInt(cu.Window), deNum(*cu.Percent, 0, 1))
	}
	sb.WriteString(", Auto-Kompaktierung " + onOff(c.AutoCompact))
	if c.AutoCompact && cu != nil && cu.ThresholdTokens > 0 {
		sb.WriteString(" ab " + fmtInt(cu.ThresholdTokens))
	}
	switch {
	case c.Compactions == 1:
		sb.WriteString(", 1 Kompaktierung")
	case c.Compactions > 1:
		fmt.Fprintf(&sb, ", %d Kompaktierungen", c.Compactions)
	}
	return sb.String()
}

// fmtWorkspace: „Arbeitsbereich gesichert: 1,2 MB, 14 Dateien (2026-09-29 17:05)".
func fmtWorkspace(w *agwclient.Workspace) string {
	var sb strings.Builder
	if w == nil || w.SavedAt == "" {
		sb.WriteString("Arbeitsbereich noch nicht gesichert")
	} else {
		files := fmt.Sprintf("%d Dateien", w.Files)
		if w.Files == 1 {
			files = "1 Datei"
		}
		fmt.Fprintf(&sb, "Arbeitsbereich gesichert: %s MB, %s (%s)", deNum(float64(w.Size)/(1<<20), 1, 1), files, fmtTime(w.SavedAt))
	}
	if w != nil && w.SkippedReason != "" {
		fmt.Fprintf(&sb, "; zuletzt NICHT gesichert (%s): %s", fmtTime(w.SkippedAt), w.SkippedReason)
	}
	return sb.String()
}

func compactionReason(r string) string {
	switch r {
	case "manual":
		return "manuell"
	case "threshold":
		return "Schwelle erreicht"
	case "overflow":
		return "Kontext übergelaufen"
	}
	return r
}

// fmtCompacted: „Kontext zusammengefasst: 42.000 → ca. 3.100 Tokens".
func fmtCompacted(reason string, before, after int64) string {
	head := "Kontext zusammengefasst"
	if reason != "" {
		head += " (" + compactionReason(reason) + ")"
	}
	switch {
	case before > 0 && after > 0:
		return fmt.Sprintf("%s: %s → ca. %s Tokens", head, fmtInt(before), fmtInt(after))
	case before > 0:
		return fmt.Sprintf("%s: vorher %s Tokens", head, fmtInt(before))
	}
	return head
}

func commandSource(s string) string {
	switch s {
	case "builtin":
		return "eingebaut"
	case "extension":
		return "Erweiterung"
	case "prompt":
		return "Vorlage"
	case "skill":
		return "Skill"
	}
	return s
}

func tariffLabel(peak bool) string {
	if peak {
		return "Spitzentarif"
	}
	return "Nebentarif"
}

var dayLabels = map[string]string{"mon": "Mo", "tue": "Di", "wed": "Mi", "thu": "Do", "fri": "Fr", "sat": "Sa", "sun": "So"}

// fmtDays übersetzt „mon-fri" → „Mo–Fr", „sat,sun" → „Sa, So", „all" → „täglich".
func fmtDays(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" || d == "all" {
		return "täglich"
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

// fmtWindows: „Mo–Fr 01:00–09:00 UTC; Sa 02:00–04:00 UTC".
func fmtWindows(ws []agwclient.TariffWindow) string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%s %s–%s UTC", fmtDays(w.Days), w.From, w.To))
	}
	return strings.Join(out, "; ")
}

// oneLine fasst Leerraum einschließlich Zeilenumbrüchen zu einfachen Leerzeichen zusammen.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstLine: erste Zeile gekürzt, dazu die Zahl weiterer Zeilen.
func firstLine(s string, max int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := truncate(lines[0], max)
	if len(lines) > 1 {
		out += fmt.Sprintf(" … (+%d Zeilen)", len(lines)-1)
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
		return "Haupt"
	}
	return "Sub"
}

// fmtStatus: 200 → „ok", 429 → „429 abgewiesen", sonst die Zahl.
func fmtStatus(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "ok"
	case code == 429:
		return "429 abgewiesen"
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

// fmtLLMCallLine: „Modellaufruf Sub · ein 500, aus 100, Cache 0 · 0,0012 USD · Werkzeuge bash · ok".
func fmtLLMCallLine(c agwclient.LLMCall) string {
	s := fmt.Sprintf("Modellaufruf %s · ein %s, aus %s, Cache %s · %s", mainLabel(c.Main),
		fmtInt(c.Input), fmtInt(c.Output), fmtInt(c.CacheRead), fmtCost(c.Cost))
	if t := llmToolNames(c); t != "" {
		s += " · Werkzeuge " + t
	}
	return s + " · " + fmtStatus(c.Status)
}

// fmtClock: Uhrzeit (lokal) eines RFC-3339-Zeitpunkts.
func fmtClock(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("15:04:05")
}

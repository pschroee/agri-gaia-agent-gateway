// Command agw ist das Kommandozeilenwerkzeug für den Orchestrator des PoC (siehe poc/API.md).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"agw/internal/agwclient"
)

const usageText = `agw – Kommandozeile für den Orchestrator

Aufruf: agw [--url URL] [--json] <befehl> [argumente]

Befehle:
  models                        wählbare Modelle mit Preisen
  variants                      Anbindungsvarianten
  config                        Voreinstellungen des Servers
  pool                          Plätze im Pool, Ziel/frei/vergeben, Summen
  chat list                     alle Chats, neueste zuerst
  chat new [--model M] [--variant cli|mcp|beide] [--title T] [--internet=true|false] [--max-subagents N] [nachricht]
  chat show <id> [--thinking]   Kopf und Verlauf
  chat send <id> <text> [--wait] [--auto-approve|--auto-reject] [--thinking] [--verbose]
                                arbeitet der Agent gerade, wird die Nachricht eingereiht
  chat queue <id> [--send]      eingereihte Nachrichten; --send übergibt sie jetzt
  chat unqueue <id> <eintrag>   eingereihte Nachricht entfernen (solange nicht übergeben)
  chat bg <id> [--tail N]       Hintergrundaufgaben (bash mit run_in_background), letzte Zeilen
  chat bg-stop <id> <bg-id>     laufende Hintergrundaufgabe beenden (der Agent wird benachrichtigt)
  chat suspend|abort <id>
  chat internet <id> on|off
  chat autocompact <id> on|off  automatische Kompaktierung ein/aus
  chat subagents <id> [N]       Subagenten-Läufe anzeigen, mit N die Grenze setzen
  chat calls <id>               Modellaufrufe laut LLM-Proxy mit Kosten
  chat execs <id> [--flagged]   Werkzeugausführungen des Orchestrators, abgeglichen mit dem Proxy
  chat commands <id>            Slash-Befehle des Chats
  chat cmd <id> "/befehl …" [--wait] [--auto-approve|--auto-reject] [--thinking]
  chat compact <id> [anweisungen] [--wait]   Kontext jetzt zusammenfassen
  chat upload <id> <datei>...
  chat artifacts <id>
  chat download <id> <name> [--kind input|output] [-o datei]
  chat session <id>             Sitzungsdatei von pi (JSONL)
  approvals [--chat id] [--all] offene (oder alle) Bestätigungen
  approve <id> | reject <id>
  watch <chat-id> [--verbose]   Ereignisse live mitlesen (Strg+C beendet)
  run [--model M] [--variant V] [--internet=…] [--max-subagents N] [--auto-approve|--auto-reject] [--thinking] [--verbose] "<aufgabe>"

--verbose zeigt zusätzlich jeden Modellaufruf (LLM-Proxy) mit Kosten und die Eingriffe der Subagenten-Grenze.

Adresse: --url, sonst AGW_URL, sonst ` + agwclient.DefaultURL + `
`

// usageError führt zu Exit-Code 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

type app struct {
	ctx    context.Context
	c      *agwclient.Client
	url    string
	json   bool
	stdin  *bufio.Reader
	stdout io.Writer
	stderr io.Writer
	color  bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(realMain(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func realMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	a := &app{ctx: ctx, stdin: bufio.NewReader(stdin), stdout: stdout, stderr: stderr}
	a.color = isTerminal(stderr) && getenv("NO_COLOR") == ""
	a.url = getenv("AGW_URL")
	if a.url == "" {
		a.url = agwclient.DefaultURL
	}
	top := flag.NewFlagSet("agw", flag.ContinueOnError)
	top.SetOutput(stderr)
	top.Usage = func() { io.WriteString(stderr, usageText) }
	top.StringVar(&a.url, "url", a.url, "Adresse des Orchestrators")
	top.BoolVar(&a.json, "json", false, "maschinenlesbare Ausgabe")
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := top.Args()
	if len(rest) == 0 {
		io.WriteString(stderr, usageText)
		return 2
	}
	err := a.dispatch(rest[0], rest[1:])
	return a.exitCode(err)
}

func (a *app) exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(a.stderr, "agw: %s\n", ue.msg)
		return 2
	}
	if errors.Is(err, errFlagParse) {
		return 2
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(a.stderr, "agw: abgebrochen")
		return 130
	}
	if errors.Is(err, errSilentFailure) {
		return 1
	}
	fmt.Fprintf(a.stderr, "agw: %v\n", err)
	return 1
}

// errSilentFailure: der Fehler wurde bereits ausgegeben.
var errSilentFailure = errors.New("fehlgeschlagen")

// errFlagParse: das flag-Paket hat die Meldung schon geschrieben.
var errFlagParse = errors.New("ungültige Argumente")

func (a *app) dispatch(cmd string, args []string) error {
	switch cmd {
	case "models":
		return a.cmdModels(args)
	case "variants":
		return a.cmdVariants(args)
	case "config":
		return a.cmdConfig(args)
	case "pool":
		return a.cmdPool(args)
	case "chat", "chats":
		if len(args) == 0 {
			return usagef("chat: Unterbefehl fehlt (list, new, show, send, queue, unqueue, suspend, abort, internet, autocompact, subagents, calls, commands, cmd, compact, upload, artifacts, download, session)")
		}
		return a.dispatchChat(args[0], args[1:])
	case "approvals":
		return a.cmdApprovals(args)
	case "approve":
		return a.cmdDecide("approve", args, true)
	case "reject":
		return a.cmdDecide("reject", args, false)
	case "watch":
		return a.cmdWatch(args)
	case "run":
		return a.cmdRun(args)
	case "help", "-h", "--help":
		io.WriteString(a.stdout, usageText)
		return nil
	}
	return usagef("Unbekannter Befehl %q – agw help zeigt alle Befehle", cmd)
}

func (a *app) dispatchChat(sub string, args []string) error {
	switch sub {
	case "list", "ls":
		return a.cmdChatList(args)
	case "new":
		return a.cmdChatNew(args)
	case "show":
		return a.cmdChatShow(args)
	case "send":
		return a.cmdChatSend(args)
	case "queue":
		return a.cmdChatQueue(args)
	case "unqueue":
		return a.cmdChatUnqueue(args)
	case "bg":
		return a.cmdChatBg(args)
	case "bg-stop":
		return a.cmdChatBgStop(args)
	case "suspend", "abort":
		return a.cmdChatAction(sub, args)
	case "internet":
		return a.cmdChatInternet(args)
	case "autocompact":
		return a.cmdChatAutocompact(args)
	case "subagents":
		return a.cmdChatSubagents(args)
	case "calls", "llm_calls":
		return a.cmdChatCalls(args)
	case "execs", "executions", "tool_executions":
		return a.cmdChatExecs(args)
	case "commands":
		return a.cmdChatCommands(args)
	case "cmd", "command":
		return a.cmdChatCmd(args)
	case "compact":
		return a.cmdChatCompact(args)
	case "upload":
		return a.cmdChatUpload(args)
	case "artifacts":
		return a.cmdChatArtifacts(args)
	case "download":
		return a.cmdChatDownload(args)
	case "session":
		return a.cmdChatSession(args)
	}
	return usagef("Unbekannter Befehl \"chat %s\" – agw help zeigt alle Befehle", sub)
}

// flags legt ein FlagSet mit den gemeinsamen Schaltern --url und --json an.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("agw "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.StringVar(&a.url, "url", a.url, "Adresse des Orchestrators")
	fs.BoolVar(&a.json, "json", a.json, "maschinenlesbare Ausgabe")
	return fs
}

// parse erlaubt Schalter auch zwischen und nach den Positionsargumenten („send <id> text --wait").
// Alles nach „--" ist positional.
func (a *app) parse(fs *flag.FlagSet, args []string, minPos, maxPos int, usage string) ([]string, error) {
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "Aufruf: %s\n", usage)
		fs.PrintDefaults()
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errFlagParse
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	if len(pos) < minPos || (maxPos >= 0 && len(pos) > maxPos) {
		return nil, usagef("Aufruf: %s", usage)
	}
	a.c = agwclient.New(a.url)
	return pos, nil
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (a *app) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
}

func (a *app) dim(s string) string {
	if !a.color {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

// triBool ist ein Schalter, der wissen lässt, ob er überhaupt gesetzt wurde.
type triBool struct {
	set bool
	val bool
}

func (t *triBool) String() string {
	if t == nil || !t.set {
		return ""
	}
	return fmt.Sprint(t.val)
}

func (t *triBool) Set(s string) error {
	switch strings.ToLower(s) {
	case "true", "1", "on", "an", "ja", "j", "yes":
		t.val = true
	case "false", "0", "off", "aus", "nein", "n", "no":
		t.val = false
	default:
		return fmt.Errorf("true oder false erwartet, nicht %q", s)
	}
	t.set = true
	return nil
}

func (t *triBool) IsBoolFlag() bool { return true }

// optInt ist eine nicht negative Zahl, von der man weiß, ob sie gesetzt wurde.
type optInt struct {
	set bool
	val int
}

func (o *optInt) String() string {
	if o == nil || !o.set {
		return ""
	}
	return fmt.Sprint(o.val)
}

func (o *optInt) Set(s string) error {
	n, err := parseCount(s)
	if err != nil {
		return err
	}
	o.val, o.set = n, true
	return nil
}

func (o *optInt) ptr() *int {
	if !o.set {
		return nil
	}
	v := o.val
	return &v
}

// parseCount liest eine ganze Zahl ≥ 0.
func parseCount(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("ganze Zahl ≥ 0 erwartet, nicht %q", s)
	}
	return n, nil
}

func (t *triBool) ptr() *bool {
	if !t.set {
		return nil
	}
	v := t.val
	return &v
}

// ---- Stammdaten -----------------------------------------------------------------------------

func (a *app) cmdModels(args []string) error {
	fs := a.flags("models")
	if _, err := a.parse(fs, args, 0, 0, "agw models [--json]"); err != nil {
		return err
	}
	ms, err := a.c.Models(a.ctx)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(ms)
	}
	tw := a.table()
	fmt.Fprintln(tw, "Kennung\tName\tStandard\tjetzt\tEingabe\tAusgabe\tCache lesen\tCache schreiben\tHinweis")
	var tariffs []string
	for _, m := range ms {
		in, out, cr, cw, note := "–", "–", "–", "–", ""
		t := m.Tariff
		if t != nil && len(t.PeakWindowsUTC) == 0 {
			t = nil
		}
		price := func(v float64) string {
			if t == nil || v == 0 {
				return fmtPrice(v)
			}
			return fmtPrice(v) + " (neben " + fmtPrice(v*t.OffpeakFactor) + ")"
		}
		if p := m.Pricing; p != nil {
			in, out, cr, cw, note = price(p.Input), price(p.Output), price(p.CacheRead), price(p.CacheWrite), p.Note
		}
		std := ""
		if m.Default {
			std = "ja"
		}
		now := ""
		if t != nil {
			now = "Neben"
			if m.PeakNow != nil && *m.PeakNow {
				now = "Spitze"
			}
			line := fmt.Sprintf("%s: Spitzentarif %s, sonst Nebentarif (Faktor %s)", m.ID, fmtWindows(t.PeakWindowsUTC), deNum(t.OffpeakFactor, 0, 3))
			if t.Note != "" {
				line += " – " + t.Note
			}
			tariffs = append(tariffs, line)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Name, std, now, in, out, cr, cw, note)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, l := range tariffs {
		fmt.Fprintln(a.stdout, l)
	}
	fmt.Fprintln(a.stdout, a.dim("Preise in USD je 1 Mio. Tokens; bei Tarifen ist der Spitzentarif angegeben, der Nebentarif in Klammern."))
	return nil
}

func (a *app) cmdVariants(args []string) error {
	fs := a.flags("variants")
	if _, err := a.parse(fs, args, 0, 0, "agw variants [--json]"); err != nil {
		return err
	}
	vs, err := a.c.Variants(a.ctx)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(vs)
	}
	tw := a.table()
	fmt.Fprintln(tw, "Kennung\tBezeichnung\tWerkzeuge")
	for _, v := range vs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", v.ID, v.Label, strings.Join(v.Tools, ", "))
	}
	return tw.Flush()
}

func (a *app) cmdConfig(args []string) error {
	fs := a.flags("config")
	if _, err := a.parse(fs, args, 0, 0, "agw config [--json]"); err != nil {
		return err
	}
	c, err := a.c.Config(a.ctx)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	tw := a.table()
	fmt.Fprintf(tw, "Internet für neue Chats\t%s\n", onOff(c.InternetDefault))
	fmt.Fprintf(tw, "Frist für Bestätigungen\t%s s\n", deNum(c.ApprovalTimeoutS, 0, 1))
	fmt.Fprintf(tw, "Größte Artefaktdatei\t%s MB\n", deNum(c.ArtifactMaxMB, 0, 1))
	fmt.Fprintf(tw, "Ruhezeit bis zum Einschlafen\t%s s\n", deNum(c.IdleTimeoutS, 0, 1))
	fmt.Fprintf(tw, "Auto-Kompaktierung für neue Chats\t%s\n", onOff(c.AutoCompactDefault))
	fmt.Fprintf(tw, "Reserve bis zur Kompaktierung\t%s Tokens\n", fmtInt(c.CompactReserveTokens))
	fmt.Fprintf(tw, "Beim Kompaktieren behalten\t%s Tokens\n", fmtInt(c.CompactKeepRecentTokens))
	fmt.Fprintf(tw, "Subagenten für neue Chats\t%d (höchstens %d)\n", c.MaxSubagentsDefault, c.MaxSubagentsLimit)
	return tw.Flush()
}

func (a *app) cmdPool(args []string) error {
	fs := a.flags("pool")
	if _, err := a.parse(fs, args, 0, 0, "agw pool [--json]"); err != nil {
		return err
	}
	p, err := a.c.Pool(a.ctx)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(p)
	}
	type counts struct{ free, assigned, starting, stopping int }
	per := map[string]*counts{}
	get := func(v string) *counts {
		if per[v] == nil {
			per[v] = &counts{}
		}
		return per[v]
	}
	for v := range p.Targets {
		get(v)
	}
	for _, s := range p.Slots {
		c := get(s.Variant)
		switch s.State {
		case "idle":
			c.free++
		case "assigned":
			c.assigned++
		case "starting":
			c.starting++
		case "stopping":
			c.stopping++
		}
	}
	variants := make([]string, 0, len(per))
	for v := range per {
		variants = append(variants, v)
	}
	sort.Strings(variants)
	tw := a.table()
	for _, v := range variants {
		c := per[v]
		fmt.Fprintf(tw, "%s\tZiel %d\tfrei %d\tvergeben %d\tstartend %d\tstoppend %d\n", v, p.Targets[v], c.free, c.assigned, c.starting, c.stopping)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Gesamt: Kosten %s · Tokens %s · aktive Chats %d\n\n",
		fmtCost(p.Totals.Cost), fmtTokens(p.Totals.Tokens), p.Totals.ChatsActive)
	if len(p.Slots) == 0 {
		fmt.Fprintln(a.stdout, "Keine Plätze.")
		return nil
	}
	now := time.Now()
	tw = a.table()
	fmt.Fprintln(tw, "Platz\tVariante\tZustand\tContainer\tChat\tTätigkeit\tInternet\tseit")
	for _, s := range p.Slots {
		chat := ""
		if s.ChatID != "" {
			chat = shortID(s.ChatID)
			if s.ChatTitle != "" {
				chat += " " + truncate(s.ChatTitle, 30)
			}
		}
		since := s.CreatedAt
		if s.AssignedAt != "" {
			since = s.AssignedAt
		}
		if s.Activity != nil && s.Activity.Since != "" {
			since = s.Activity.Since
		}
		inet := ""
		if s.Internet != nil {
			inet = onOff(*s.Internet)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Variant, slotState(s.State), s.ContainerID,
			chat, activityLabel(s.Activity), inet, fmtSince(since, now))
	}
	return tw.Flush()
}

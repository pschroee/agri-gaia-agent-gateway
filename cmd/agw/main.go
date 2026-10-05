// Command agw is the command line tool for the PoC's orchestrator (see API.md).
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

const usageText = `agw – command line for the orchestrator

Usage: agw [--url URL] [--json] <command> [arguments]

Commands:
  models                        selectable models with prices
  variants                      binding variants
  config                        server defaults
  pool                          slots in the pool, target/free/assigned, totals
  chat list                     all chats, newest first
  chat new [--model M] [--variant cli|mcp|both] [--title T] [--internet=true|false] [--max-subagents N] [message]
  chat show <id> [--thinking]   header and history
  chat send <id> <text> [--wait] [--auto-approve|--auto-reject] [--thinking] [--verbose]
                                if the agent is working, the message is queued
  chat queue <id> [--send]      queued messages; --send hands them over now
  chat unqueue <id> <entry>     remove a queued message (as long as it has not been handed over)
  chat bg <id> [--tail N]       background tasks (bash with run_in_background), last lines
  chat bg-stop <id> <bg-id>     stop a running background task (the agent is notified)
  chat suspend|abort <id>
  chat internet <id> on|off
  chat autocompact <id> on|off  automatic compaction on/off
  chat subagents <id> [N]       show subagent runs, set the limit with N
  chat calls <id>               model calls according to the LLM proxy, with costs
  chat execs <id> [--flagged]   tool executions of the orchestrator, reconciled with the proxy
  chat commands <id>            slash commands of the chat
  chat cmd <id> "/command …" [--wait] [--auto-approve|--auto-reject] [--thinking]
  chat compact <id> [instructions] [--wait]   summarise the context now
  chat upload <id> <file>...
  chat artifacts <id>
  chat download <id> <name> [--kind input|output] [-o file]
  chat session <id>             pi's session file (JSONL)
  approvals [--chat id] [--all] pending (or all) approvals
  approve <id> | reject <id>
  watch <chat-id> [--verbose]   follow events live (Ctrl+C ends)
  run [--model M] [--variant V] [--internet=…] [--max-subagents N] [--auto-approve|--auto-reject] [--thinking] [--verbose] "<task>"

--verbose additionally shows every model call (LLM proxy) with costs and the interventions of the subagent limit.

Address: --url, else AGW_URL, else ` + agwclient.DefaultURL + `
`

// usageError leads to exit code 2.
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
	top.StringVar(&a.url, "url", a.url, "address of the orchestrator")
	top.BoolVar(&a.json, "json", false, "machine-readable output")
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
		fmt.Fprintln(a.stderr, "agw: aborted")
		return 130
	}
	if errors.Is(err, errSilentFailure) {
		return 1
	}
	fmt.Fprintf(a.stderr, "agw: %v\n", err)
	return 1
}

// errSilentFailure: the error has already been printed.
var errSilentFailure = errors.New("failed")

// errFlagParse: the flag package has already written the message.
var errFlagParse = errors.New("invalid arguments")

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
			return usagef("chat: subcommand missing (list, new, show, send, queue, unqueue, suspend, abort, internet, autocompact, subagents, calls, commands, cmd, compact, upload, artifacts, download, session)")
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
	return usagef("unknown command %q – agw help shows all commands", cmd)
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
	return usagef("unknown command \"chat %s\" – agw help shows all commands", sub)
}

// flags creates a FlagSet with the common switches --url and --json.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("agw "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.StringVar(&a.url, "url", a.url, "address of the orchestrator")
	fs.BoolVar(&a.json, "json", a.json, "machine-readable output")
	return fs
}

// parse also allows switches between and after the positional arguments ("send <id> text --wait").
// Everything after "--" is positional.
func (a *app) parse(fs *flag.FlagSet, args []string, minPos, maxPos int, usage string) ([]string, error) {
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "usage: %s\n", usage)
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
		return nil, usagef("usage: %s", usage)
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

// triBool is a switch that tells whether it was set at all.
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
	case "true", "1", "on", "yes", "y":
		t.val = true
	case "false", "0", "off", "no", "n":
		t.val = false
	default:
		return fmt.Errorf("expected true or false, not %q", s)
	}
	t.set = true
	return nil
}

func (t *triBool) IsBoolFlag() bool { return true }

// optInt is a non-negative number that knows whether it was set.
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

// parseCount reads an integer ≥ 0.
func parseCount(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("expected an integer ≥ 0, not %q", s)
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

// ---- master data ----------------------------------------------------------------------------

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
	fmt.Fprintln(tw, "ID\tName\tDefault\tnow\tInput\tOutput\tCache read\tCache write\tNote")
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
			return fmtPrice(v) + " (off-peak " + fmtPrice(v*t.OffpeakFactor) + ")"
		}
		if p := m.Pricing; p != nil {
			in, out, cr, cw, note = price(p.Input), price(p.Output), price(p.CacheRead), price(p.CacheWrite), p.Note
		}
		std := ""
		if m.Default {
			std = "yes"
		}
		now := ""
		if t != nil {
			now = "off-peak"
			if m.PeakNow != nil && *m.PeakNow {
				now = "peak"
			}
			line := fmt.Sprintf("%s: peak tariff %s, otherwise off-peak tariff (factor %s)", m.ID, fmtWindows(t.PeakWindowsUTC), fmtNum(t.OffpeakFactor, 0, 3))
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
	fmt.Fprintln(a.stdout, a.dim("Prices in USD per 1M tokens; with tariffs the peak tariff is shown, the off-peak tariff in brackets."))
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
	fmt.Fprintln(tw, "ID\tLabel\tTools")
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
	fmt.Fprintf(tw, "Internet for new chats\t%s\n", onOff(c.InternetDefault))
	fmt.Fprintf(tw, "Deadline for approvals\t%s s\n", fmtNum(c.ApprovalTimeoutS, 0, 1))
	fmt.Fprintf(tw, "Largest artifact file\t%s MB\n", fmtNum(c.ArtifactMaxMB, 0, 1))
	fmt.Fprintf(tw, "Idle time until going to sleep\t%s s\n", fmtNum(c.IdleTimeoutS, 0, 1))
	fmt.Fprintf(tw, "Auto-compaction for new chats\t%s\n", onOff(c.AutoCompactDefault))
	fmt.Fprintf(tw, "Reserve until compaction\t%s tokens\n", fmtInt(c.CompactReserveTokens))
	fmt.Fprintf(tw, "Kept when compacting\t%s tokens\n", fmtInt(c.CompactKeepRecentTokens))
	fmt.Fprintf(tw, "Subagents for new chats\t%d (at most %d)\n", c.MaxSubagentsDefault, c.MaxSubagentsLimit)
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
		fmt.Fprintf(tw, "%s\ttarget %d\tfree %d\tassigned %d\tstarting %d\tstopping %d\n", v, p.Targets[v], c.free, c.assigned, c.starting, c.stopping)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Total: cost %s · tokens %s · active chats %d\n\n",
		fmtCost(p.Totals.Cost), fmtTokens(p.Totals.Tokens), p.Totals.ChatsActive)
	if len(p.Slots) == 0 {
		fmt.Fprintln(a.stdout, "No slots.")
		return nil
	}
	now := time.Now()
	tw = a.table()
	fmt.Fprintln(tw, "Slot\tVariant\tState\tContainer\tChat\tActivity\tInternet\tsince")
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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agw/internal/agwclient"
)

func (a *app) cmdChatList(args []string) error {
	fs := a.flags("chat list")
	if _, err := a.parse(fs, args, 0, 0, "agw chat list [--json]"); err != nil {
		return err
	}
	cs, err := a.c.Chats(a.ctx)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(cs)
	}
	if len(cs) == 0 {
		fmt.Fprintln(a.stdout, "No chats yet.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "ID\tTitle\tModel\tVariant\tState\tInternet\tTokens\tCost\tArtifacts\tpending\tchanged")
	for _, c := range cs {
		st := chatState(c.State)
		if c.Running {
			st += ", running"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\t%s\n", c.ID, truncate(c.Title, 40), c.Model, c.Variant,
			st, onOff(c.Internet), c.Tokens.Total, fmtCost(c.Cost), c.ArtifactCount, c.PendingApprovals, fmtTime(c.UpdatedAt))
	}
	return tw.Flush()
}

func (a *app) chatFlags(fs *flag.FlagSet, req *agwclient.CreateChatRequest, inet *triBool, maxSub *optInt) {
	fs.StringVar(&req.Model, "model", "", "model (ID from agw models)")
	fs.StringVar(&req.Variant, "variant", "", "binding: cli, mcp, api or beide")
	fs.StringVar(&req.Title, "title", "", "title of the chat")
	fs.Var(inet, "internet", "internet access of the sandbox (true|false)")
	fs.Var(maxSub, "max-subagents", "at most this many subagents (default: server default)")
	fs.Func("delegation", "delegated rights as a JSON file (see docs/plan-delegation-rest-platform.md); - reads from stdin", func(p string) error {
		var b []byte
		var err error
		if p == "-" {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		} else {
			b, err = os.ReadFile(p)
		}
		if err != nil {
			return err
		}
		if !json.Valid(b) {
			return fmt.Errorf("%s is not valid JSON", p)
		}
		req.Delegation = b
		return nil
	})
}

func (a *app) cmdChatNew(args []string) error {
	fs := a.flags("chat new")
	var req agwclient.CreateChatRequest
	var inet triBool
	var maxSub optInt
	a.chatFlags(fs, &req, &inet, &maxSub)
	pos, err := a.parse(fs, args, 0, -1, "agw chat new [--model M] [--variant cli|mcp|api|beide] [--title T] [--internet=true|false] [--max-subagents N] [--delegation file.json] [message]")
	if err != nil {
		return err
	}
	req.Message = strings.Join(pos, " ")
	req.Internet = inet.ptr()
	req.MaxSubagents = maxSub.ptr()
	chat, err := a.c.CreateChat(a.ctx, req)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(chat)
	}
	fmt.Fprintln(a.stdout, chat.ID)
	return nil
}

func (a *app) cmdChatShow(args []string) error {
	fs := a.flags("chat show")
	thinking := fs.Bool("thinking", false, "show thinking blocks")
	summary := fs.Bool("summary", false, "show the summaries of the compactions")
	pos, err := a.parse(fs, args, 1, 1, "agw chat show <id> [--thinking] [--summary] [--json]")
	if err != nil {
		return err
	}
	det, err := a.c.Chat(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(det)
	}
	w := a.stdout
	c := det.Chat
	fmt.Fprintf(w, "Chat %s – %s\n", c.ID, c.Title)
	fmt.Fprintf(w, "model %s · variant %s · state %s · internet %s · running %s", c.Model, c.Variant, chatState(c.State), onOff(c.Internet), yesNo(c.Running))
	if c.SlotID != "" {
		fmt.Fprintf(w, " · slot %s", c.SlotID)
	}
	fmt.Fprintf(w, "\ntokens %s · %s\n", fmtTokens(c.Tokens), fmtChatCost(c))
	fmt.Fprintf(w, "subagents %d/%d · model calls %d\n", c.Subagents, c.MaxSubagents, c.LLMCalls)
	fmt.Fprintln(w, fmtContext(c))
	fmt.Fprintln(w, fmtWorkspace(c.Workspace))
	if c.Queued > 0 {
		fmt.Fprintf(w, "Queued: %d message(s)", c.Queued)
		if c.QueueHeld {
			if r := holdReasonText(c.HoldReason); r != "" {
				fmt.Fprintf(w, " – %s", r)
			}
			fmt.Fprintf(w, " – go along with the next message (agw chat queue %s --send)", c.ID)
		}
		fmt.Fprintln(w)
	}
	if c.BackgroundRunning > 0 {
		fmt.Fprintf(w, "Background tasks: %d running (agw chat bg %s)\n", c.BackgroundRunning, c.ID)
	}
	if c.CreatedAt != "" {
		fmt.Fprintf(w, "created %s · changed %s\n", fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt))
	}
	fmt.Fprintln(w)
	// Note peak/off-peak tariff only if the model has a tariff at all; without a tariff
	// the server returns peak=false, which would be misleading as "off-peak tariff".
	tariff := false
	if ms, err := a.c.Models(a.ctx); err == nil {
		for _, m := range ms {
			if m.ID == c.Model && m.Tariff != nil && len(m.Tariff.PeakWindowsUTC) > 0 {
				tariff = true
			}
		}
	}
	for _, sm := range det.Messages {
		a.printStoredMessage(sm, *thinking, *summary, tariff)
	}
	if len(det.Artifacts) > 0 {
		fmt.Fprintln(w, "\nArtifacts:")
		tw := a.table()
		for _, ar := range det.Artifacts {
			fmt.Fprintf(tw, "  %s\t%s\t%d bytes\tvia %s\t%s\n", kindLabel(ar.Kind), ar.Name, ar.Size, ar.Via, fmtTime(ar.CreatedAt))
		}
		tw.Flush()
	}
	var open []agwclient.Approval
	for _, ap := range det.Approvals {
		if ap.State == "pending" {
			open = append(open, ap)
		}
	}
	if len(open) > 0 {
		fmt.Fprintln(w, "\nPending approvals:")
		for _, ap := range open {
			fmt.Fprintf(w, "  %s  %s (via %s) – agw approve %s | agw reject %s\n", ap.ID, approvalSubject(ap), ap.Via, ap.ID, ap.ID)
		}
	}
	return nil
}

func indent(s, prefix string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+prefix)
}

// approvalSubject describes what an approval asks for.
func approvalSubject(ap agwclient.Approval) string {
	if ap.Kind == "internet_access" {
		return "Internet access: " + orDefault(ap.Name, "(no reason given)")
	}
	if ap.Kind == "platform_write" {
		return "Platform call: " + ap.Name
	}
	return fmt.Sprintf("%s (%d bytes)", ap.Name, ap.Size)
}

// fmtMsgCost: "0.0012 USD (off-peak tariff)"; empty if the server reports no costs.
func fmtMsgCost(sm agwclient.StoredMessage, tariff bool) string {
	if sm.Cost == nil {
		return ""
	}
	s := fmtCost(*sm.Cost)
	if tariff && sm.Peak != nil {
		s += " (" + tariffLabel(*sm.Peak) + ")"
	}
	return s
}

func (a *app) printStoredMessage(sm agwclient.StoredMessage, thinking, summary, tariff bool) {
	var m piMessage
	if json.Unmarshal(sm.Message, &m) != nil {
		return
	}
	role := m.Role
	if role == "" {
		role = sm.Role
	}
	w := a.stdout
	switch role {
	case "compaction":
		var cm struct {
			Reason               string `json:"reason"`
			Summary              string `json:"summary"`
			TokensBefore         int64  `json:"tokensBefore"`
			EstimatedTokensAfter int64  `json:"estimatedTokensAfter"`
		}
		_ = json.Unmarshal(sm.Message, &cm)
		line := fmtCompacted(orDefault(cm.Reason, "manual"), cm.TokensBefore, cm.EstimatedTokensAfter)
		if c := fmtMsgCost(sm, tariff); c != "" {
			line += " · " + c
		}
		fmt.Fprintf(w, "── %s ──\n", line)
		if summary && strings.TrimSpace(cm.Summary) != "" {
			fmt.Fprintf(w, "%s\n", a.dim("  "+indent(cm.Summary, "  ")))
		}
	case "user":
		fmt.Fprintf(w, "User: %s\n", indent(textOf(m.Content), "      "))
	case "assistant":
		var text []string
		for _, b := range blocks(m.Content) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					text = append(text, b.Text)
				}
			case "thinking":
				if thinking && strings.TrimSpace(b.Thinking) != "" {
					fmt.Fprintf(w, "%s\n", a.dim("  (thinking) "+indent(b.Thinking, "             ")))
				}
			case "toolCall":
				if len(text) > 0 {
					fmt.Fprintf(w, "Assistant: %s\n", indent(strings.Join(text, ""), "           "))
					text = nil
				}
				line := "  ▶ " + b.Name
				if args := compactJSON(b.Arguments, 200); args != "" {
					line += " " + args
				}
				fmt.Fprintln(w, line)
			}
		}
		if len(text) > 0 {
			fmt.Fprintf(w, "Assistant: %s\n", indent(strings.Join(text, ""), "           "))
		}
		if m.StopReason == "error" {
			fmt.Fprintf(w, "  Error: %s\n", m.ErrorMessage)
		}
		if c := fmtMsgCost(sm, tariff); c != "" {
			fmt.Fprintf(w, "%s\n", a.dim("  cost "+c))
		}
	case "toolResult":
		t := strings.TrimRight(textOf(m.Content), "\n")
		lines := strings.Split(t, "\n")
		mark := "↳"
		if m.IsError {
			mark = "✗"
		}
		first := truncate(lines[0], 160)
		if len(lines) > 1 {
			first += fmt.Sprintf(" … (+%d lines)", len(lines)-1)
		}
		fmt.Fprintf(w, "  %s %s\n", mark, a.dim(first))
	}
}

// fmtChatCost: "cost 0.0500 USD (of which 0.0100 USD outside the main replies)".
// cost comes from the LLM proxy and includes subagents and compactions.
func fmtChatCost(c agwclient.Chat) string {
	s := "cost " + fmtCost(c.Cost)
	if c.CostOther > 0 {
		s += " (of which " + fmtCost(c.CostOther) + " outside the main replies)"
	}
	return s
}

// streamOpts: common switches for send --wait, cmd --wait and run.
type streamOpts struct {
	auto, reject, thinking, verbose *bool
}

func approvalFlags(fs *flag.FlagSet) streamOpts {
	return streamOpts{
		auto:     fs.Bool("auto-approve", false, "approve artifacts and internet access automatically"),
		reject:   fs.Bool("auto-reject", false, "reject artifacts and internet access automatically"),
		thinking: fs.Bool("thinking", false, "print thinking dimmed on stderr"),
		verbose:  fs.Bool("verbose", false, "show every model call with costs and the interventions of the subagent limit"),
	}
}

func (a *app) newStreamer(chatID string, o streamOpts) (*streamer, error) {
	s, err := a.newStreamerMode(chatID, *o.auto, *o.reject, *o.thinking)
	if err == nil {
		s.calls = *o.verbose
	}
	return s, err
}

func (a *app) newStreamerMode(chatID string, auto, reject, thinking bool) (*streamer, error) {
	if auto && reject {
		return nil, usagef("--auto-approve and --auto-reject are mutually exclusive")
	}
	s := newStreamer(a.stdout, a.stderr)
	s.in = a.stdin
	s.color = a.color
	s.thinking = thinking
	s.chatID = chatID
	s.decide = a.c.Decide
	s.mode = approvalAsk
	if auto {
		s.mode = approvalAuto
	} else if reject {
		s.mode = approvalReject
	}
	return s, nil
}

func (a *app) cmdChatSend(args []string) error {
	fs := a.flags("chat send")
	wait := fs.Bool("wait", false, "print the reply live until pi is done")
	o := approvalFlags(fs)
	pos, err := a.parse(fs, args, 2, -1, "agw chat send <id> <text> [--wait] [--auto-approve|--auto-reject] [--thinking] [--verbose]")
	if err != nil {
		return err
	}
	id, text := pos[0], strings.Join(pos[1:], " ")
	if !*wait {
		if *o.auto || *o.reject {
			return usagef("--auto-approve/--auto-reject only work with --wait")
		}
		res, err := a.c.Send(a.ctx, id, text)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(res)
		}
		switch {
		case res.Queued:
			fmt.Fprintf(a.stdout, "Queued (%s); goes to the agent as soon as the running turn ends.\n", res.QueueID)
		case res.Resumed:
			fmt.Fprintln(a.stdout, "Sent; the chat was resumed in a fresh sandbox.")
		default:
			fmt.Fprintln(a.stdout, "Sent.")
		}
		return nil
	}
	s, err := a.newStreamer(id, o)
	if err != nil {
		return err
	}
	return a.streamToEnd(id, text, s, true)
}

// streamToEnd sends, prints the reply and ends with a totals line on stderr.
func (a *app) streamToEnd(id, text string, s *streamer, checkRunning bool) error {
	return a.actToEnd(id, s, checkRunning, func(ctx context.Context) (agwclient.SendResult, error) {
		return a.c.Send(ctx, id, text)
	})
}

// actToEnd: like streamToEnd, but with any action (message or slash command).
func (a *app) actToEnd(id string, s *streamer, checkRunning bool, act func(context.Context) (agwclient.SendResult, error)) error {
	_, err := actAndFollow(a.ctx, a.c, id, s, checkRunning, act)
	if err != nil {
		if a.ctx.Err() != nil {
			fmt.Fprintf(a.stderr, "\nAborted. The agent may keep working: agw chat abort %s\n", id)
		}
		return err
	}
	if det, err := a.c.Chat(a.ctx, id); err == nil {
		c := det.Chat
		line := fmt.Sprintf("chat %s · tokens %s · %s", c.ID, fmtTokens(c.Tokens), fmtChatCost(c))
		if c.Subagents > 0 {
			line += fmt.Sprintf(" · subagents %d/%d", c.Subagents, c.MaxSubagents)
		}
		fmt.Fprintf(a.stderr, "%s\n", a.dim(line))
		fmt.Fprintf(a.stderr, "%s\n", a.dim(fmtContext(c)))
	} else {
		fmt.Fprintf(a.stderr, "chat %s (totals not available: %v)\n", id, err)
	}
	if s.failed {
		return errSilentFailure
	}
	return nil
}

func (a *app) cmdChatAction(action string, args []string) error {
	fs := a.flags("chat " + action)
	pos, err := a.parse(fs, args, 1, 1, "agw chat "+action+" <id> [--json]")
	if err != nil {
		return err
	}
	var c agwclient.Chat
	var msg string
	switch action {
	case "suspend":
		c, err = a.c.Suspend(a.ctx, pos[0])
		msg = "is idle; the sandbox has been removed"
	case "abort":
		c, err = a.c.Abort(a.ctx, pos[0])
		msg = "– running reply aborted"
	}
	if err != nil {
		var ae *agwclient.APIError
		if action == "suspend" && errors.As(err, &ae) && ae.Status == 409 {
			return fmt.Errorf("chat cannot go idle while an approval is pending (agw approvals --chat %s): %s", pos[0], ae.Message)
		}
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	fmt.Fprintf(a.stdout, "Chat %s %s (state %s).\n", c.ID, msg, chatState(c.State))
	return nil
}

func (a *app) cmdChatInternet(args []string) error {
	fs := a.flags("chat internet")
	pos, err := a.parse(fs, args, 2, 2, "agw chat internet <id> on|off")
	if err != nil {
		return err
	}
	var t triBool
	if t.Set(pos[1]) != nil {
		return usagef("usage: agw chat internet <id> on|off")
	}
	c, err := a.c.SetInternet(a.ctx, pos[0], t.val)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	when := "immediately"
	if c.State != "active" {
		when = "on the next resume"
	}
	fmt.Fprintf(a.stdout, "Internet for chat %s %s (takes effect %s).\n", c.ID, onOff(c.Internet), when)
	return nil
}

func (a *app) printArtifacts(arts []agwclient.Artifact) error {
	if len(arts) == 0 {
		fmt.Fprintln(a.stdout, "No artifacts.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "Kind\tName\tSize\tType\tvia\tSHA-256\tcreated")
	for _, ar := range arts {
		sha := ar.SHA256
		if len(sha) > 12 {
			sha = sha[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", kindLabel(ar.Kind), ar.Name, ar.Size, ar.ContentType, ar.Via, sha, fmtTime(ar.CreatedAt))
	}
	return tw.Flush()
}

func (a *app) cmdChatUpload(args []string) error {
	fs := a.flags("chat upload")
	pos, err := a.parse(fs, args, 2, -1, "agw chat upload <id> <file>...")
	if err != nil {
		return err
	}
	arts, err := a.c.Upload(a.ctx, pos[0], pos[1:])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(arts)
	}
	if err := a.printArtifacts(arts); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, a.dim("In the sandbox under /workspace/inputs/."))
	return nil
}

func (a *app) cmdChatArtifacts(args []string) error {
	fs := a.flags("chat artifacts")
	pos, err := a.parse(fs, args, 1, 1, "agw chat artifacts <id> [--json]")
	if err != nil {
		return err
	}
	arts, err := a.c.Artifacts(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(arts)
	}
	return a.printArtifacts(arts)
}

func (a *app) cmdChatDownload(args []string) error {
	fs := a.flags("chat download")
	kind := fs.String("kind", "output", "input or output")
	out := fs.String("o", "", "target file (default: name of the artifact, - for stdout)")
	pos, err := a.parse(fs, args, 2, 2, "agw chat download <id> <name> [--kind input|output] [-o file]")
	if err != nil {
		return err
	}
	if *kind != "input" && *kind != "output" {
		return usagef("--kind must be input or output")
	}
	id, name := pos[0], pos[1]
	if *out == "-" {
		_, err := a.c.Download(a.ctx, id, name, *kind, a.stdout)
		return err
	}
	dst := *out
	if dst == "" {
		dst = filepath.Base(name)
		if dst == "." || dst == "/" || dst == ".." {
			return usagef("no usable file name – please give -o")
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".agw-download-*")
	if err != nil {
		return err
	}
	n, err := a.c.Download(a.ctx, id, name, *kind, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	fmt.Fprintf(a.stderr, "%d bytes written to %s.\n", n, dst)
	return nil
}

func (a *app) cmdChatSession(args []string) error {
	fs := a.flags("chat session")
	pos, err := a.parse(fs, args, 1, 1, "agw chat session <id>")
	if err != nil {
		return err
	}
	return a.c.Session(a.ctx, pos[0], a.stdout)
}

// ---- compaction and slash commands ----------------------------------------------------------

func (a *app) cmdChatAutocompact(args []string) error {
	fs := a.flags("chat autocompact")
	pos, err := a.parse(fs, args, 2, 2, "agw chat autocompact <id> on|off")
	if err != nil {
		return err
	}
	var t triBool
	if t.Set(pos[1]) != nil {
		return usagef("usage: agw chat autocompact <id> on|off")
	}
	c, err := a.c.SetAutoCompact(a.ctx, pos[0], t.val)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	fmt.Fprintf(a.stdout, "Auto-compaction for chat %s %s.\n", c.ID, onOff(c.AutoCompact))
	return nil
}

func (a *app) cmdChatCommands(args []string) error {
	fs := a.flags("chat commands")
	pos, err := a.parse(fs, args, 1, 1, "agw chat commands <id> [--json]")
	if err != nil {
		return err
	}
	cmds, err := a.c.Commands(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		if cmds == nil {
			cmds = []agwclient.Command{}
		}
		return a.printJSON(cmds)
	}
	if len(cmds) == 0 {
		fmt.Fprintln(a.stdout, "No commands.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "Name\tSource\tDescription")
	for _, c := range cmds {
		name := "/" + c.Name
		if c.Args != "" {
			name += " " + c.Args
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, commandSource(c.Source), truncate(strings.Join(strings.Fields(c.Description), " "), 100))
	}
	return tw.Flush()
}

func (a *app) cmdChatCmd(args []string) error {
	fs := a.flags("chat cmd")
	wait := fs.Bool("wait", false, "wait until the command is done (for /compact until the compaction ends)")
	o := approvalFlags(fs)
	usage := "agw chat cmd <id> \"/command …\" [--wait] [--auto-approve|--auto-reject] [--thinking] [--verbose]"
	pos, err := a.parse(fs, args, 2, -1, usage)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(strings.Join(pos[1:], " "))
	if !strings.HasPrefix(line, "/") || len(line) < 2 {
		return usagef("a command starts with \"/\" (agw chat commands %s shows all) – usage: %s", pos[0], usage)
	}
	return a.runCommand(pos[0], line, *wait, o)
}

func (a *app) cmdChatCompact(args []string) error {
	fs := a.flags("chat compact")
	wait := fs.Bool("wait", false, "wait until the compaction is done")
	pos, err := a.parse(fs, args, 1, -1, "agw chat compact <id> [instructions] [--wait]")
	if err != nil {
		return err
	}
	line := "/compact"
	if instr := strings.TrimSpace(strings.Join(pos[1:], " ")); instr != "" {
		line += " " + instr
	}
	f := false
	return a.runCommand(pos[0], line, *wait, streamOpts{&f, &f, &f, &f})
}

// commandError phrases the 409 for /compact understandably.
func commandError(id, name string, err error) error {
	var ae *agwclient.APIError
	if name == "compact" && errors.As(err, &ae) && ae.Status == 409 {
		return fmt.Errorf("the agent is working right now; /compact only works afterwards (agw chat abort %s aborts)", id)
	}
	return err
}

// runCommand runs a slash command. With wait it subscribes first and follows until
// compaction_end for /compact, otherwise until agent_settled; /autocompact delivers no events.
func (a *app) runCommand(id, line string, wait bool, o streamOpts) error {
	name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	act := func(ctx context.Context) (agwclient.SendResult, error) {
		r, err := a.c.RunCommand(ctx, id, line)
		if err != nil {
			return agwclient.SendResult{}, commandError(id, name, err)
		}
		return agwclient.SendResult{OK: r.OK, Resumed: r.Resumed, Queued: r.Queued, QueueID: r.QueueID}, nil
	}
	if !wait || name == "autocompact" {
		if *o.auto || *o.reject {
			return usagef("--auto-approve/--auto-reject only work with --wait")
		}
		res, err := a.c.RunCommand(a.ctx, id, line)
		if err != nil {
			return commandError(id, name, err)
		}
		if a.json {
			return a.printJSON(res)
		}
		switch name {
		case "compact":
			fmt.Fprintf(a.stdout, "Compaction started – agw chat show %s shows the result, --wait waits for it.\n", id)
		case "autocompact":
			fmt.Fprintln(a.stdout, "Auto-compaction toggled.")
		default:
			if res.Queued {
				fmt.Fprintln(a.stdout, "Command queued; goes to the agent as soon as the running turn ends.")
			} else {
				fmt.Fprintln(a.stdout, "Command sent.")
			}
		}
		if res.Resumed {
			fmt.Fprintln(a.stdout, "The chat was resumed in a fresh sandbox.")
		}
		return nil
	}
	s, err := a.newStreamer(id, o)
	if err != nil {
		return err
	}
	s.untilCompaction = name == "compact"
	return a.actToEnd(id, s, name != "compact", act)
}

// ---- approvals ------------------------------------------------------------------------------

func (a *app) cmdApprovals(args []string) error {
	fs := a.flags("approvals")
	chat := fs.String("chat", "", "only approvals of this chat")
	all := fs.Bool("all", false, "also decided and expired ones")
	if _, err := a.parse(fs, args, 0, 0, "agw approvals [--chat id] [--all] [--json]"); err != nil {
		return err
	}
	state := "pending"
	if *all {
		state = ""
	}
	aps, err := a.c.Approvals(a.ctx, state)
	if err != nil {
		return err
	}
	if *chat != "" {
		filtered := aps[:0]
		for _, ap := range aps {
			if ap.ChatID == *chat {
				filtered = append(filtered, ap)
			}
		}
		aps = filtered
	}
	if a.json {
		if aps == nil {
			aps = []agwclient.Approval{}
		}
		return a.printJSON(aps)
	}
	if len(aps) == 0 {
		if *all {
			fmt.Fprintln(a.stdout, "No approvals.")
		} else {
			fmt.Fprintln(a.stdout, "No pending approvals.")
		}
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "ID\tChat\tName\tSize\tvia\tState\tcreated")
	for _, ap := range aps {
		name, size := ap.Name, fmt.Sprint(ap.Size)
		if ap.Kind == "internet_access" {
			name, size = "Internet access: "+truncate(orDefault(ap.Name, "(no reason given)"), 60), "–"
		}
		if ap.Kind == "platform_write" {
			name = "Platform call: " + truncate(ap.Name, 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ap.ID, ap.ChatID, name, size, ap.Via, approvalState(ap.State), fmtTime(ap.CreatedAt))
	}
	return tw.Flush()
}

func (a *app) cmdDecide(name string, args []string, approve bool) error {
	fs := a.flags(name)
	pos, err := a.parse(fs, args, 1, 1, "agw "+name+" <id>")
	if err != nil {
		return err
	}
	ap, err := a.c.Decide(a.ctx, pos[0], approve)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(ap)
	}
	label := ap.Name
	if label == "" {
		label = ap.ID
	}
	fmt.Fprintf(a.stdout, "Approval %s (%s): %s\n", ap.ID, label, approvalState(ap.State))
	return nil
}

// ---- Live -----------------------------------------------------------------------------------

func (a *app) cmdWatch(args []string) error {
	fs := a.flags("watch")
	thinking := fs.Bool("thinking", false, "print thinking dimmed")
	verbose := fs.Bool("verbose", false, "also show every model call with costs")
	pos, err := a.parse(fs, args, 1, 1, "agw watch <chat-id> [--thinking] [--verbose]")
	if err != nil {
		return err
	}
	body, err := a.c.Events(a.ctx, pos[0])
	if err != nil {
		return err
	}
	defer body.Close()
	s := newStreamer(a.stdout, a.stderr)
	s.color, s.thinking, s.verbose, s.calls, s.chatID, s.mode = a.color, *thinking, true, *verbose, pos[0], approvalShow
	fmt.Fprintln(a.stderr, a.dim("Following chat "+pos[0]+" – Ctrl+C ends."))
	err = follow(a.ctx, body, s, nil, false)
	if a.ctx.Err() != nil {
		return nil
	}
	if err == nil {
		return errors.New("the server ended the event stream")
	}
	return err
}

func (a *app) cmdRun(args []string) error {
	fs := a.flags("run")
	var req agwclient.CreateChatRequest
	var inet triBool
	var maxSub optInt
	a.chatFlags(fs, &req, &inet, &maxSub)
	o := approvalFlags(fs)
	pos, err := a.parse(fs, args, 1, -1, "agw run [--model M] [--variant cli|mcp|api|beide] [--internet=true|false] [--max-subagents N] [--delegation file.json] [--auto-approve|--auto-reject] [--thinking] [--verbose] \"<task>\"")
	if err != nil {
		return err
	}
	if *o.auto && *o.reject {
		return usagef("--auto-approve and --auto-reject are mutually exclusive")
	}
	task := strings.Join(pos, " ")
	if strings.TrimSpace(task) == "" {
		return usagef("the task is empty")
	}
	if req.Title == "" {
		req.Title = truncate(strings.Join(strings.Fields(task), " "), 60)
	}
	req.Internet = inet.ptr()
	req.MaxSubagents = maxSub.ptr()
	// Create without a message: subscribe first, then send – otherwise the first events would be lost.
	chat, err := a.c.CreateChat(a.ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stderr, a.dim(fmt.Sprintf("chat %s · model %s · variant %s · internet %s · subagents at most %d", chat.ID, chat.Model, chat.Variant, onOff(chat.Internet), chat.MaxSubagents)))
	s, err := a.newStreamer(chat.ID, o)
	if err != nil {
		return err
	}
	return a.streamToEnd(chat.ID, task, s, false)
}

// cmdChatQueue shows the queued messages; --send hands them over now.
func (a *app) cmdChatQueue(args []string) error {
	fs := a.flags("chat queue")
	send := fs.Bool("send", false, "hand held-back messages to the agent now")
	pos, err := a.parse(fs, args, 1, 1, "agw chat queue <id> [--send] [--json]")
	if err != nil {
		return err
	}
	if *send {
		res, err := a.c.FlushQueue(a.ctx, pos[0])
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(res)
		}
		fmt.Fprintln(a.stdout, "Queued messages handed over.")
		return nil
	}
	q, err := a.c.Queue(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(q)
	}
	if len(q) == 0 {
		fmt.Fprintln(a.stdout, "No queued messages.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "ENTRY\tTIME\tTEXT\tATTACHMENTS")
	for _, e := range q {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, fmtTime(e.CreatedAt), truncateLine(e.Text, 60), strings.Join(e.Attachments, ", "))
	}
	return tw.Flush()
}

// cmdChatUnqueue removes a queued message as long as it has not been handed over.
func (a *app) cmdChatUnqueue(args []string) error {
	fs := a.flags("chat unqueue")
	pos, err := a.parse(fs, args, 2, 2, "agw chat unqueue <id> <entry>")
	if err != nil {
		return err
	}
	if err := a.c.Unqueue(a.ctx, pos[0], pos[1]); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Removed from the queue.")
	return nil
}

// truncateLine truncates to n characters and turns line breaks into spaces.
func truncateLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " …"
	}
	return s
}

// holdReasonText explains why queued messages are held back (hold_reason on the chat).
func holdReasonText(r string) string {
	switch r {
	case "abort":
		return "held after an abort"
	case "wake_limit":
		return "limit of wake-ups per hour reached"
	case "auto_turns":
		return "limit of turns without the user reached"
	}
	return ""
}

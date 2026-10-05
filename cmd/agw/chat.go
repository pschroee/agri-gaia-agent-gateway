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
		fmt.Fprintln(a.stdout, "Noch keine Chats.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "ID\tTitel\tModell\tVariante\tZustand\tInternet\tTokens\tKosten\tArtefakte\toffen\tgeändert")
	for _, c := range cs {
		st := chatState(c.State)
		if c.Running {
			st += ", läuft"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\t%s\n", c.ID, truncate(c.Title, 40), c.Model, c.Variant,
			st, onOff(c.Internet), c.Tokens.Total, fmtCost(c.Cost), c.ArtifactCount, c.PendingApprovals, fmtTime(c.UpdatedAt))
	}
	return tw.Flush()
}

func (a *app) chatFlags(fs *flag.FlagSet, req *agwclient.CreateChatRequest, inet *triBool, maxSub *optInt) {
	fs.StringVar(&req.Model, "model", "", "Modell (Kennung aus agw models)")
	fs.StringVar(&req.Variant, "variant", "", "Anbindung: cli, mcp, api oder beide")
	fs.StringVar(&req.Title, "title", "", "Titel des Chats")
	fs.Var(inet, "internet", "Internetzugang der Sandbox (true|false)")
	fs.Var(maxSub, "max-subagents", "höchstens so viele Subagenten (Standard: Voreinstellung des Servers)")
	fs.Func("delegation", "übertragene Rechte als JSON-Datei (siehe docs/plan-delegation-rest-plattform.md); - liest von stdin", func(p string) error {
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
			return fmt.Errorf("%s ist kein gültiges JSON", p)
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
	pos, err := a.parse(fs, args, 0, -1, "agw chat new [--model M] [--variant cli|mcp|api|beide] [--title T] [--internet=true|false] [--max-subagents N] [--delegation datei.json] [nachricht]")
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
	thinking := fs.Bool("thinking", false, "Thinking-Blöcke anzeigen")
	summary := fs.Bool("summary", false, "Zusammenfassungen der Kompaktierungen anzeigen")
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
	fmt.Fprintf(w, "Modell %s · Variante %s · Zustand %s · Internet %s · läuft %s", c.Model, c.Variant, chatState(c.State), onOff(c.Internet), yesNo(c.Running))
	if c.SlotID != "" {
		fmt.Fprintf(w, " · Platz %s", c.SlotID)
	}
	fmt.Fprintf(w, "\nTokens %s · %s\n", fmtTokens(c.Tokens), fmtChatCost(c))
	fmt.Fprintf(w, "Subagenten %d/%d · Modellaufrufe %d\n", c.Subagents, c.MaxSubagents, c.LLMCalls)
	fmt.Fprintln(w, fmtContext(c))
	fmt.Fprintln(w, fmtWorkspace(c.Workspace))
	if c.Queued > 0 {
		fmt.Fprintf(w, "Eingereiht: %d Nachricht(en)", c.Queued)
		if c.QueueHeld {
			if r := holdReasonText(c.HoldReason); r != "" {
				fmt.Fprintf(w, " – %s", r)
			}
			fmt.Fprintf(w, " – gehen mit der nächsten Nachricht mit (agw chat queue %s --send)", c.ID)
		}
		fmt.Fprintln(w)
	}
	if c.BackgroundRunning > 0 {
		fmt.Fprintf(w, "Hintergrundaufgaben: %d laufen (agw chat bg %s)\n", c.BackgroundRunning, c.ID)
	}
	if c.CreatedAt != "" {
		fmt.Fprintf(w, "angelegt %s · geändert %s\n", fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt))
	}
	fmt.Fprintln(w)
	// Spitzen-/Nebentarif nur vermerken, wenn das Modell überhaupt einen Tarif hat; ohne Tarif
	// liefert der Server peak=false, das wäre als „Nebentarif" irreführend.
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
		fmt.Fprintln(w, "\nArtefakte:")
		tw := a.table()
		for _, ar := range det.Artifacts {
			fmt.Fprintf(tw, "  %s\t%s\t%d Bytes\tüber %s\t%s\n", kindLabel(ar.Kind), ar.Name, ar.Size, ar.Via, fmtTime(ar.CreatedAt))
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
		fmt.Fprintln(w, "\nOffene Bestätigungen:")
		for _, ap := range open {
			fmt.Fprintf(w, "  %s  %s (über %s) – agw approve %s | agw reject %s\n", ap.ID, approvalSubject(ap), ap.Via, ap.ID, ap.ID)
		}
	}
	return nil
}

func indent(s, prefix string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+prefix)
}

// approvalSubject beschreibt, worum eine Bestätigung bittet.
func approvalSubject(ap agwclient.Approval) string {
	if ap.Kind == "internet_access" {
		return "Internetzugang: " + orDefault(ap.Name, "(ohne Begründung)")
	}
	if ap.Kind == "platform_write" {
		return "Plattform-Aufruf: " + ap.Name
	}
	return fmt.Sprintf("%s (%d Bytes)", ap.Name, ap.Size)
}

// fmtMsgCost: „0,0012 USD (Nebentarif)"; leer, wenn der Server keine Kosten nennt.
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
		fmt.Fprintf(w, "Nutzer: %s\n", indent(textOf(m.Content), "        "))
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
					fmt.Fprintf(w, "%s\n", a.dim("  (denkt) "+indent(b.Thinking, "          ")))
				}
			case "toolCall":
				if len(text) > 0 {
					fmt.Fprintf(w, "Assistent: %s\n", indent(strings.Join(text, ""), "           "))
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
			fmt.Fprintf(w, "Assistent: %s\n", indent(strings.Join(text, ""), "           "))
		}
		if m.StopReason == "error" {
			fmt.Fprintf(w, "  Fehler: %s\n", m.ErrorMessage)
		}
		if c := fmtMsgCost(sm, tariff); c != "" {
			fmt.Fprintf(w, "%s\n", a.dim("  Kosten "+c))
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
			first += fmt.Sprintf(" … (+%d Zeilen)", len(lines)-1)
		}
		fmt.Fprintf(w, "  %s %s\n", mark, a.dim(first))
	}
}

// fmtChatCost: „Kosten 0,0500 USD (davon 0,0100 USD außerhalb der Hauptantworten)".
// cost stammt vom LLM-Proxy und schließt Subagenten und Kompaktierungen ein.
func fmtChatCost(c agwclient.Chat) string {
	s := "Kosten " + fmtCost(c.Cost)
	if c.CostOther > 0 {
		s += " (davon " + fmtCost(c.CostOther) + " außerhalb der Hauptantworten)"
	}
	return s
}

// streamOpts: gemeinsame Schalter für send --wait, cmd --wait und run.
type streamOpts struct {
	auto, reject, thinking, verbose *bool
}

func approvalFlags(fs *flag.FlagSet) streamOpts {
	return streamOpts{
		auto:     fs.Bool("auto-approve", false, "Artefakte und Internetzugang automatisch bestätigen"),
		reject:   fs.Bool("auto-reject", false, "Artefakte und Internetzugang automatisch ablehnen"),
		thinking: fs.Bool("thinking", false, "Thinking gedimmt auf stderr ausgeben"),
		verbose:  fs.Bool("verbose", false, "jeden Modellaufruf mit Kosten und Eingriffe der Subagenten-Grenze zeigen"),
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
		return nil, usagef("--auto-approve und --auto-reject schließen sich aus")
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
	wait := fs.Bool("wait", false, "Antwort live ausgeben, bis pi fertig ist")
	o := approvalFlags(fs)
	pos, err := a.parse(fs, args, 2, -1, "agw chat send <id> <text> [--wait] [--auto-approve|--auto-reject] [--thinking] [--verbose]")
	if err != nil {
		return err
	}
	id, text := pos[0], strings.Join(pos[1:], " ")
	if !*wait {
		if *o.auto || *o.reject {
			return usagef("--auto-approve/--auto-reject wirken nur mit --wait")
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
			fmt.Fprintf(a.stdout, "Eingereiht (%s); geht an den Agenten, sobald der laufende Durchgang endet.\n", res.QueueID)
		case res.Resumed:
			fmt.Fprintln(a.stdout, "Gesendet; der Chat wurde in einer frischen Sandbox fortgesetzt.")
		default:
			fmt.Fprintln(a.stdout, "Gesendet.")
		}
		return nil
	}
	s, err := a.newStreamer(id, o)
	if err != nil {
		return err
	}
	return a.streamToEnd(id, text, s, true)
}

// streamToEnd sendet, gibt die Antwort aus und schließt mit einer Summenzeile auf stderr.
func (a *app) streamToEnd(id, text string, s *streamer, checkRunning bool) error {
	return a.actToEnd(id, s, checkRunning, func(ctx context.Context) (agwclient.SendResult, error) {
		return a.c.Send(ctx, id, text)
	})
}

// actToEnd: wie streamToEnd, aber mit beliebiger Aktion (Nachricht oder Slash-Befehl).
func (a *app) actToEnd(id string, s *streamer, checkRunning bool, act func(context.Context) (agwclient.SendResult, error)) error {
	_, err := actAndFollow(a.ctx, a.c, id, s, checkRunning, act)
	if err != nil {
		if a.ctx.Err() != nil {
			fmt.Fprintf(a.stderr, "\nAbgebrochen. Der Agent arbeitet ggf. weiter: agw chat abort %s\n", id)
		}
		return err
	}
	if det, err := a.c.Chat(a.ctx, id); err == nil {
		c := det.Chat
		line := fmt.Sprintf("Chat %s · Tokens %s · %s", c.ID, fmtTokens(c.Tokens), fmtChatCost(c))
		if c.Subagents > 0 {
			line += fmt.Sprintf(" · Subagenten %d/%d", c.Subagents, c.MaxSubagents)
		}
		fmt.Fprintf(a.stderr, "%s\n", a.dim(line))
		fmt.Fprintf(a.stderr, "%s\n", a.dim(fmtContext(c)))
	} else {
		fmt.Fprintf(a.stderr, "Chat %s (Summen nicht abrufbar: %v)\n", id, err)
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
		msg = "ruht; die Sandbox ist abgebaut"
	case "abort":
		c, err = a.c.Abort(a.ctx, pos[0])
		msg = "– laufende Antwort abgebrochen"
	}
	if err != nil {
		var ae *agwclient.APIError
		if action == "suspend" && errors.As(err, &ae) && ae.Status == 409 {
			return fmt.Errorf("Chat kann nicht ruhen, solange eine Bestätigung offen ist (agw approvals --chat %s): %s", pos[0], ae.Message)
		}
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	fmt.Fprintf(a.stdout, "Chat %s %s (Zustand %s).\n", c.ID, msg, chatState(c.State))
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
		return usagef("Aufruf: agw chat internet <id> on|off")
	}
	c, err := a.c.SetInternet(a.ctx, pos[0], t.val)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	when := "sofort"
	if c.State != "active" {
		when = "beim nächsten Fortsetzen"
	}
	fmt.Fprintf(a.stdout, "Internet für Chat %s %s (wirkt %s).\n", c.ID, onOff(c.Internet), when)
	return nil
}

func (a *app) printArtifacts(arts []agwclient.Artifact) error {
	if len(arts) == 0 {
		fmt.Fprintln(a.stdout, "Keine Artefakte.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "Art\tName\tGröße\tTyp\tüber\tSHA-256\tangelegt")
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
	pos, err := a.parse(fs, args, 2, -1, "agw chat upload <id> <datei>...")
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
	fmt.Fprintln(a.stdout, a.dim("In der Sandbox unter /workspace/inputs/."))
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
	kind := fs.String("kind", "output", "input oder output")
	out := fs.String("o", "", "Zieldatei (Standard: Name des Artefakts, - für stdout)")
	pos, err := a.parse(fs, args, 2, 2, "agw chat download <id> <name> [--kind input|output] [-o datei]")
	if err != nil {
		return err
	}
	if *kind != "input" && *kind != "output" {
		return usagef("--kind muss input oder output sein")
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
			return usagef("Kein brauchbarer Dateiname – bitte -o angeben")
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
	fmt.Fprintf(a.stderr, "%d Bytes nach %s geschrieben.\n", n, dst)
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

// ---- Kompaktierung und Slash-Befehle --------------------------------------------------------

func (a *app) cmdChatAutocompact(args []string) error {
	fs := a.flags("chat autocompact")
	pos, err := a.parse(fs, args, 2, 2, "agw chat autocompact <id> on|off")
	if err != nil {
		return err
	}
	var t triBool
	if t.Set(pos[1]) != nil {
		return usagef("Aufruf: agw chat autocompact <id> on|off")
	}
	c, err := a.c.SetAutoCompact(a.ctx, pos[0], t.val)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(c)
	}
	fmt.Fprintf(a.stdout, "Auto-Kompaktierung für Chat %s %s.\n", c.ID, onOff(c.AutoCompact))
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
		fmt.Fprintln(a.stdout, "Keine Befehle.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "Name\tQuelle\tBeschreibung")
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
	wait := fs.Bool("wait", false, "warten, bis der Befehl fertig ist (bei /compact bis zum Ende der Kompaktierung)")
	o := approvalFlags(fs)
	usage := "agw chat cmd <id> \"/befehl …\" [--wait] [--auto-approve|--auto-reject] [--thinking] [--verbose]"
	pos, err := a.parse(fs, args, 2, -1, usage)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(strings.Join(pos[1:], " "))
	if !strings.HasPrefix(line, "/") || len(line) < 2 {
		return usagef("Ein Befehl beginnt mit „/\" (agw chat commands %s zeigt alle) – Aufruf: %s", pos[0], usage)
	}
	return a.runCommand(pos[0], line, *wait, o)
}

func (a *app) cmdChatCompact(args []string) error {
	fs := a.flags("chat compact")
	wait := fs.Bool("wait", false, "warten, bis die Kompaktierung fertig ist")
	pos, err := a.parse(fs, args, 1, -1, "agw chat compact <id> [anweisungen] [--wait]")
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

// commandError formuliert den 409 bei /compact verständlich.
func commandError(id, name string, err error) error {
	var ae *agwclient.APIError
	if name == "compact" && errors.As(err, &ae) && ae.Status == 409 {
		return fmt.Errorf("Der Agent arbeitet gerade; /compact geht erst danach (agw chat abort %s bricht ab)", id)
	}
	return err
}

// runCommand führt einen Slash-Befehl aus. Mit wait wird vorher abonniert und bei /compact bis
// compaction_end, sonst bis agent_settled mitgelesen; /autocompact liefert keine Ereignisse.
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
			return usagef("--auto-approve/--auto-reject wirken nur mit --wait")
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
			fmt.Fprintf(a.stdout, "Kompaktierung gestartet – agw chat show %s zeigt das Ergebnis, --wait wartet darauf.\n", id)
		case "autocompact":
			fmt.Fprintln(a.stdout, "Auto-Kompaktierung umgeschaltet.")
		default:
			if res.Queued {
				fmt.Fprintln(a.stdout, "Befehl eingereiht; geht an den Agenten, sobald der laufende Durchgang endet.")
			} else {
				fmt.Fprintln(a.stdout, "Befehl gesendet.")
			}
		}
		if res.Resumed {
			fmt.Fprintln(a.stdout, "Der Chat wurde in einer frischen Sandbox fortgesetzt.")
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

// ---- Bestätigungen --------------------------------------------------------------------------

func (a *app) cmdApprovals(args []string) error {
	fs := a.flags("approvals")
	chat := fs.String("chat", "", "nur Bestätigungen dieses Chats")
	all := fs.Bool("all", false, "auch entschiedene und abgelaufene")
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
			fmt.Fprintln(a.stdout, "Keine Bestätigungen.")
		} else {
			fmt.Fprintln(a.stdout, "Keine offenen Bestätigungen.")
		}
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "ID\tChat\tName\tGröße\tüber\tZustand\tangelegt")
	for _, ap := range aps {
		name, size := ap.Name, fmt.Sprint(ap.Size)
		if ap.Kind == "internet_access" {
			name, size = "Internetzugang: "+truncate(orDefault(ap.Name, "(ohne Begründung)"), 60), "–"
		}
		if ap.Kind == "platform_write" {
			name = "Plattform-Aufruf: " + truncate(ap.Name, 60)
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
	fmt.Fprintf(a.stdout, "Bestätigung %s (%s): %s\n", ap.ID, label, approvalState(ap.State))
	return nil
}

// ---- Live -----------------------------------------------------------------------------------

func (a *app) cmdWatch(args []string) error {
	fs := a.flags("watch")
	thinking := fs.Bool("thinking", false, "Thinking gedimmt ausgeben")
	verbose := fs.Bool("verbose", false, "auch jeden Modellaufruf mit Kosten zeigen")
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
	fmt.Fprintln(a.stderr, a.dim("Lese Chat "+pos[0]+" mit – Strg+C beendet."))
	err = follow(a.ctx, body, s, nil, false)
	if a.ctx.Err() != nil {
		return nil
	}
	if err == nil {
		return errors.New("Der Server hat den Ereignisstrom beendet")
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
	pos, err := a.parse(fs, args, 1, -1, "agw run [--model M] [--variant cli|mcp|api|beide] [--internet=true|false] [--max-subagents N] [--delegation datei.json] [--auto-approve|--auto-reject] [--thinking] [--verbose] \"<aufgabe>\"")
	if err != nil {
		return err
	}
	if *o.auto && *o.reject {
		return usagef("--auto-approve und --auto-reject schließen sich aus")
	}
	task := strings.Join(pos, " ")
	if strings.TrimSpace(task) == "" {
		return usagef("Die Aufgabe ist leer")
	}
	if req.Title == "" {
		req.Title = truncate(strings.Join(strings.Fields(task), " "), 60)
	}
	req.Internet = inet.ptr()
	req.MaxSubagents = maxSub.ptr()
	// Ohne message anlegen: erst abonnieren, dann senden – sonst gingen die ersten Ereignisse verloren.
	chat, err := a.c.CreateChat(a.ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stderr, a.dim(fmt.Sprintf("Chat %s · Modell %s · Variante %s · Internet %s · Subagenten höchstens %d", chat.ID, chat.Model, chat.Variant, onOff(chat.Internet), chat.MaxSubagents)))
	s, err := a.newStreamer(chat.ID, o)
	if err != nil {
		return err
	}
	return a.streamToEnd(chat.ID, task, s, false)
}

// cmdChatQueue zeigt die eingereihten Nachrichten; --send übergibt sie jetzt.
func (a *app) cmdChatQueue(args []string) error {
	fs := a.flags("chat queue")
	send := fs.Bool("send", false, "zurückgehaltene Nachrichten jetzt an den Agenten übergeben")
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
		fmt.Fprintln(a.stdout, "Eingereihte Nachrichten übergeben.")
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
		fmt.Fprintln(a.stdout, "Keine eingereihten Nachrichten.")
		return nil
	}
	tw := a.table()
	fmt.Fprintln(tw, "EINTRAG\tZEIT\tTEXT\tANHÄNGE")
	for _, e := range q {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, fmtTime(e.CreatedAt), truncateLine(e.Text, 60), strings.Join(e.Attachments, ", "))
	}
	return tw.Flush()
}

// cmdChatUnqueue entfernt eine eingereihte Nachricht, solange sie nicht übergeben ist.
func (a *app) cmdChatUnqueue(args []string) error {
	fs := a.flags("chat unqueue")
	pos, err := a.parse(fs, args, 2, 2, "agw chat unqueue <id> <eintrag>")
	if err != nil {
		return err
	}
	if err := a.c.Unqueue(a.ctx, pos[0], pos[1]); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Aus der Warteschlange entfernt.")
	return nil
}

// truncateLine kürzt auf n Zeichen und macht Zeilenumbrüche zu Leerzeichen.
func truncateLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " …"
	}
	return s
}

// holdReasonText erklärt, warum Eingereihtes zurückgehalten ist (hold_reason am Chat).
func holdReasonText(r string) string {
	switch r {
	case "abort":
		return "angehalten nach Abbruch"
	case "wake_limit":
		return "Grenze der Weckrufe je Stunde erreicht"
	case "auto_turns":
		return "Grenze der Durchgänge ohne Nutzer erreicht"
	}
	return ""
}

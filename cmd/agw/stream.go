package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"agw/internal/agwclient"
)

// approvalMode legt fest, wie der Stream-Verarbeiter auf offene Bestätigungen reagiert.
type approvalMode int

const (
	approvalShow   approvalMode = iota // nur anzeigen (watch)
	approvalAsk                        // auf dem Terminal fragen
	approvalAuto                       // automatisch bestätigen
	approvalReject                     // automatisch ablehnen
)

// streamer setzt die SSE-Ereignisse eines Chats in Terminalausgabe um: Antworttext auf out,
// alles andere (Werkzeuge, Thinking, Bestätigungen, Fehler) auf errw.
type streamer struct {
	out, errw io.Writer
	in        *bufio.Reader
	color     bool
	thinking  bool // Thinking-Deltas ausgeben
	verbose   bool // auch Nutzernachrichten und Socket-Aufrufe (watch)
	calls     bool // jeden Modellaufruf (llm_call) und Eingriffe der Subagenten-Grenze zeigen (--verbose)
	mode      approvalMode
	chatID    string
	decide    func(ctx context.Context, id string, approve bool) (agwclient.Approval, error)
	// untilCompaction: das Warten endet mit dem ersten compaction_end nach dem Senden
	// (agw chat compact --wait); dessen Ergebnis geht dann auf out.
	untilCompaction bool

	sawStart bool // agent_start nach dem Senden gesehen
	outMid   bool // stdout steht mitten in einer Zeile
	errMid   bool // stderr steht mitten in einer Thinking-Zeile
	streamed bool // aktuelle Antwort hatte Text-Deltas
	role     string
	handled  map[string]bool
	failed   bool
}

func newStreamer(out, errw io.Writer) *streamer {
	return &streamer{out: out, errw: errw, handled: map[string]bool{}}
}

func (s *streamer) dim(t string) string {
	if !s.color {
		return t
	}
	return "\x1b[2m" + t + "\x1b[0m"
}

// breakLines schließt offene Zeilen, bevor eine Meldung auf stderr folgt.
func (s *streamer) breakLines() {
	if s.outMid {
		io.WriteString(s.out, "\n")
		s.outMid = false
	}
	if s.errMid {
		io.WriteString(s.errw, "\n")
		s.errMid = false
	}
}

func (s *streamer) note(format string, a ...any) {
	s.breakLines()
	fmt.Fprintf(s.errw, format+"\n", a...)
}

func (s *streamer) writeText(t string) {
	if t == "" {
		return
	}
	if s.errMid {
		io.WriteString(s.errw, "\n")
		s.errMid = false
	}
	io.WriteString(s.out, t)
	s.outMid = !strings.HasSuffix(t, "\n")
}

type piEvent struct {
	Type    string          `json:"type"`
	Message *piMessage      `json:"message"`
	Update  *assistantEvent `json:"assistantMessageEvent"`
	// Werkzeugausführung
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
	// compaction_start/compaction_end (Ergebnis in Result)
	Reason  string `json:"reason"`
	Aborted bool   `json:"aborted"`
	// auto_retry_start
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	ErrorMessage string `json:"errorMessage"`
}

type assistantEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// blocks liest Nachrichteninhalt, der eine Zeichenkette oder eine Blockliste sein kann.
func blocks(raw json.RawMessage) []contentBlock {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return []contentBlock{{Type: "text", Text: str}}
	}
	var bs []contentBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func textOf(raw json.RawMessage) string {
	var sb strings.Builder
	for _, b := range blocks(raw) {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image":
			sb.WriteString("[Bild]")
		}
	}
	return sb.String()
}

// resultText: Ergebnis einer Werkzeugausführung ({content:[…]}, Blockliste oder Text).
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil && len(obj.Content) > 0 {
		return textOf(obj.Content)
	}
	return textOf(raw)
}

// compactJSON gibt JSON einzeilig und gekürzt aus.
func compactJSON(raw json.RawMessage, max int) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return truncate(string(raw), max)
	}
	return truncate(buf.String(), max)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

const (
	resultLines   = 6
	resultLineMax = 200
)

func (s *streamer) toolResult(text string, isError bool) {
	text = strings.TrimRight(text, "\n")
	if isError {
		s.note("  %s", s.dim("✗ Fehler"))
	}
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	shown := lines
	if len(lines) > resultLines {
		shown = lines[:resultLines]
	}
	for _, l := range shown {
		s.note("  %s", s.dim("│ "+truncate(l, resultLineMax)))
	}
	if n := len(lines) - len(shown); n > 0 {
		s.note("  %s", s.dim(fmt.Sprintf("│ … (%d weitere Zeilen)", n)))
	}
}

// handle verarbeitet ein Ereignis. afterSend sagt, ob es nach dem Senden der eigenen
// Nachricht eintraf. done ist true beim ersten agent_settled nach einem agent_start, der
// nach dem Senden kam.
func (s *streamer) handle(ctx context.Context, ev agwclient.Event, afterSend bool) (done bool, err error) {
	switch ev.Kind {
	case "pi":
		var p piEvent
		if json.Unmarshal(ev.Data, &p) != nil {
			return false, nil
		}
		return s.handlePi(p, afterSend), nil
	case "approval":
		var a agwclient.Approval
		if json.Unmarshal(ev.Data, &a) != nil {
			return false, nil
		}
		return false, s.handleApproval(ctx, a)
	case "artifact":
		var a agwclient.Artifact
		if json.Unmarshal(ev.Data, &a) == nil {
			s.note("%s", s.dim(fmt.Sprintf("Artefakt gespeichert: %s (%d Bytes, %s)", a.Name, a.Size, kindLabel(a.Kind))))
		}
	case "error":
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		s.failed = true
		s.note("Fehler: %s", e.Message)
		if s.untilCompaction && afterSend {
			return true, nil
		}
	case "chat":
	case "resume":
		var r resumeStep
		if json.Unmarshal(ev.Data, &r) == nil {
			if line := resumeLine(r); line != "" {
				s.note("%s", s.dim(line))
			}
		}
	case "queue":
		var q struct {
			Change string   `json:"change"`
			IDs    []string `json:"ids"`
		}
		if json.Unmarshal(ev.Data, &q) == nil && q.Change == "delivered" {
			s.note("%s", s.dim(fmt.Sprintf("Eingereihte Nachrichten übergeben (%d).", len(q.IDs))))
		}
	case "socket_call":
		var c agwclient.SocketCall
		if json.Unmarshal(ev.Data, &c) == nil {
			s.socketCall(c)
		}
	case "subagent":
		var e agwclient.SubagentEntry
		if json.Unmarshal(ev.Data, &e) == nil && (s.chatID == "" || e.ChatID == "" || e.ChatID == s.chatID) {
			s.subagent(e)
		}
	case "llm_call":
		if s.calls {
			var c agwclient.LLMCall
			if json.Unmarshal(ev.Data, &c) == nil {
				s.note("%s", s.dim("· "+fmtLLMCallLine(c)))
			}
		}
	}
	return false, nil
}

// socketCall: Eingriffe der Subagenten-Grenze melden zusätzlich ein error-Ereignis und stehen
// deshalb nur mit --verbose (oder bei watch) hier; abgelehnte Rückfragen von Erweiterungen
// erscheinen sonst nirgends und werden immer gezeigt.
func (s *streamer) socketCall(c agwclient.SocketCall) {
	switch c.Op {
	case "agent_limit":
		if s.calls || s.verbose {
			s.note("%s", s.dim(fmt.Sprintf("· Proxy hat einen Modellaufruf abgewiesen (%s)", c.Detail)))
		}
	case "subagent_limit":
		if s.calls || s.verbose {
			s.note("%s", s.dim(fmt.Sprintf("· Grenze für Subagenten überschritten (%s) – Durchgang %s", c.Detail, orDefault(c.Result, "abgebrochen"))))
		}
	case "extension_ui":
		s.note("%s", s.dim(fmt.Sprintf("· Rückfrage einer Erweiterung %s (keine Eingabe möglich): %s", orDefault(c.Result, "abgelehnt"), truncate(c.Detail, 120))))
	default:
		if s.verbose {
			s.note("%s", s.dim(fmt.Sprintf("· Socket %s %s %s → %s", c.Via, c.Op, truncate(c.Detail, 80), truncate(c.Result, 80))))
		}
	}
}

// subagent gibt einen Eintrag aus einer Subagenten-Sitzung eingerückt aus.
func (s *streamer) subagent(e agwclient.SubagentEntry) {
	prefix := "  ↳ " + orDefault(e.Agent, "Subagent") + ": "
	p := e.Payload
	switch e.Kind {
	case "task":
		s.note("%s", prefix+"Auftrag: "+truncate(oneLine(p.Text), 120))
	case "tool_call":
		line := "▶ " + p.Name
		if a := compactJSON(json.RawMessage(p.Arguments), 200); a != "" {
			line += " " + a
		}
		s.note("%s", prefix+line)
	case "tool_result":
		s.note("%s", s.dim(prefix+resultMark(p.IsError)+" "+firstLine(p.Text, 160)))
	case "text":
		s.note("%s", s.dim(prefix+truncate(oneLine(p.Text), 160)))
	}
}

func (s *streamer) handlePi(p piEvent, afterSend bool) bool {
	switch p.Type {
	case "agent_start":
		if afterSend {
			s.sawStart = true
		}
	case "agent_settled":
		if afterSend && s.sawStart {
			s.breakLines()
			return true
		}
	case "message_start":
		if p.Message != nil {
			s.role = p.Message.Role
		}
		s.streamed = false
	case "message_update":
		if p.Update == nil {
			break
		}
		switch p.Update.Type {
		case "text_delta":
			s.streamed = true
			s.writeText(p.Update.Delta)
		case "thinking_delta":
			if s.thinking && p.Update.Delta != "" {
				if s.outMid {
					io.WriteString(s.out, "\n")
					s.outMid = false
				}
				io.WriteString(s.errw, s.dim(p.Update.Delta))
				s.errMid = !strings.HasSuffix(p.Update.Delta, "\n")
			}
		}
	case "message_end":
		if p.Message == nil {
			break
		}
		switch p.Message.Role {
		case "assistant":
			if !s.streamed {
				s.writeText(textOf(p.Message.Content))
			}
			s.breakLines()
			switch p.Message.StopReason {
			case "error":
				s.failed = true
				s.note("Fehler: %s", orDefault(p.Message.ErrorMessage, "Die Antwort brach mit einem Fehler ab."))
			case "aborted":
				s.note("Antwort abgebrochen.")
			}
		case "user":
			if s.verbose {
				s.breakLines()
				fmt.Fprintf(s.out, "> %s\n", strings.ReplaceAll(textOf(p.Message.Content), "\n", "\n> "))
			}
		}
		s.streamed = false
		s.role = ""
	case "tool_execution_start":
		line := "▶ " + p.ToolName
		if a := compactJSON(p.Args, 200); a != "" {
			line += " " + a
		}
		s.note("%s", line)
	case "tool_execution_end":
		s.toolResult(resultText(p.Result), p.IsError)
	case "compaction_start":
		msg := "Kontext wird zusammengefasst"
		if p.Reason != "" {
			msg += " (" + compactionReason(p.Reason) + ")"
		}
		s.note("%s", s.dim(msg+" …"))
	case "compaction_end":
		return s.compactionEnd(p, afterSend)
	case "auto_retry_start":
		msg := "Erneuter Versuch"
		if p.Attempt > 0 {
			msg += fmt.Sprintf(" %d/%d", p.Attempt, p.MaxAttempts)
		}
		if p.ErrorMessage != "" {
			msg += ": " + p.ErrorMessage
		}
		s.note("%s", s.dim(msg))
	}
	return false
}

// compactionResult: die für die Ausgabe nötigen Felder aus compaction_end.result.
type compactionResult struct {
	TokensBefore         int64 `json:"tokensBefore"`
	EstimatedTokensAfter int64 `json:"estimatedTokensAfter"`
}

func (s *streamer) compactionEnd(p piEvent, afterSend bool) bool {
	waiting := s.untilCompaction && afterSend
	var res *compactionResult
	if len(p.Result) > 0 && string(p.Result) != "null" {
		var r compactionResult
		if json.Unmarshal(p.Result, &r) == nil {
			res = &r
		}
	}
	if p.Aborted || res == nil {
		msg := "Kompaktierung abgebrochen"
		if !p.Aborted {
			msg = "Kompaktierung ohne Ergebnis"
		}
		if p.ErrorMessage != "" {
			msg += ": " + p.ErrorMessage
		}
		s.note("%s", msg)
		if waiting {
			s.failed = true
		}
		return waiting
	}
	msg := fmtCompacted("", res.TokensBefore, res.EstimatedTokensAfter)
	if waiting {
		s.breakLines()
		fmt.Fprintln(s.out, msg)
		return true
	}
	s.note("%s", s.dim(msg))
	return false
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *streamer) handleApproval(ctx context.Context, a agwclient.Approval) error {
	if s.chatID != "" && a.ChatID != "" && a.ChatID != s.chatID {
		return nil
	}
	if a.State != "pending" {
		if !s.handled[a.ID+"/"+a.State] && !s.handled[a.ID] {
			s.handled[a.ID+"/"+a.State] = true
			s.note("Bestätigung %s (%s): %s", a.ID, a.Name, approvalState(a.State))
		}
		return nil
	}
	if s.handled[a.ID] {
		return nil
	}
	s.handled[a.ID] = true
	internet := a.Kind == "internet_access"
	desc := fmt.Sprintf("Artefakt %s (%d Bytes)", a.Name, a.Size)
	question := desc + " bestätigen? [j/n] "
	if internet {
		desc = "Internetzugang"
		question = "Internetzugang erlauben? [j/n] "
	}
	if a.Kind == "platform_write" {
		desc = "Plattform-Aufruf " + a.Name
		question = desc + " ausführen? [j/n] "
	}
	request := "Agent bittet um Internetzugang: " + orDefault(a.Name, "(ohne Begründung)")
	var approve bool
	switch s.mode {
	case approvalShow:
		what := desc
		if internet {
			what = request
		}
		s.note("Offene Bestätigung %s: %s über %s – agw approve %s | agw reject %s", a.ID, what, a.Via, a.ID, a.ID)
		return nil
	case approvalAuto:
		approve = true
	case approvalReject:
		approve = false
	}
	if internet {
		s.note("%s", request)
	}
	if s.mode == approvalAsk {
		if a.Preview != "" && !internet {
			s.note("%s", s.dim("Vorschau:"))
			lines := strings.Split(strings.TrimRight(a.Preview, "\n"), "\n")
			for i, l := range lines {
				// Einen Plattform-Aufruf zeigt die Konsole ganz: bestätigt wird, was zu sehen ist (Review W1).
				if i == 10 && a.Kind != "platform_write" {
					s.note("  %s", s.dim("…"))
					break
				}
				if a.Kind == "platform_write" {
					s.note("  %s", l)
					continue
				}
				s.note("  %s", s.dim(truncate(l, resultLineMax)))
			}
		}
		var ok bool
		approve, ok = s.ask(question)
		if !ok {
			s.note("Keine Eingabe – Bestätigung bleibt offen: agw approve %s | agw reject %s", a.ID, a.ID)
			return nil
		}
	}
	if s.decide == nil {
		return errors.New("keine Entscheidungsfunktion gesetzt")
	}
	res, err := s.decide(ctx, a.ID, approve)
	if err != nil {
		s.failed = true
		s.note("Fehler beim Entscheiden über %s: %v", a.ID, err)
		return nil
	}
	word := "abgelehnt"
	if approve {
		word = "bestätigt"
	}
	if s.mode == approvalAuto || s.mode == approvalReject {
		s.note("%s automatisch %s.", desc, word)
	} else {
		s.note("%s %s.", desc, word)
	}
	if res.State != "" && res.State != "pending" {
		s.handled[a.ID+"/"+res.State] = true
	}
	return nil
}

// ask fragt auf stderr und liest die Antwort von in. ok ist false am Eingabeende.
func (s *streamer) ask(prompt string) (approve, ok bool) {
	if s.in == nil {
		return false, false
	}
	for {
		s.breakLines()
		io.WriteString(s.errw, prompt)
		line, err := s.in.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		switch ans {
		case "j", "ja", "y", "yes":
			return true, true
		case "n", "nein", "no":
			return false, true
		}
		if err != nil {
			io.WriteString(s.errw, "\n")
			return false, false
		}
		io.WriteString(s.errw, "Bitte j oder n eingeben.\n")
	}
}

type stamped struct {
	ev    agwclient.Event
	after bool
}

// startReader liest den SSE-Strom nebenläufig. Jedes Ereignis wird beim Eintreffen mit dem
// Stand von armed gestempelt (nil = immer nach dem Senden).
func startReader(ctx context.Context, r io.Reader, armed *atomic.Bool) (<-chan stamped, <-chan error) {
	ch := make(chan stamped, 1024)
	errc := make(chan error, 1)
	go func() {
		err := agwclient.ReadEvents(r, func(ev agwclient.Event) error {
			after := armed == nil || armed.Load()
			select {
			case ch <- stamped{ev, after}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		errc <- err
		close(ch)
	}()
	return ch, errc
}

var errStreamClosed = errors.New("Verbindung zum Orchestrator abgebrochen, bevor die Antwort fertig war")

// consume verarbeitet Ereignisse, bis (bei untilSettled) die Antwort fertig ist, der Strom
// endet oder ctx abgebrochen wird.
func consume(ctx context.Context, ch <-chan stamped, errc <-chan error, s *streamer, untilSettled bool) error {
	for {
		select {
		case <-ctx.Done():
			s.breakLines()
			return ctx.Err()
		case st, ok := <-ch:
			if !ok {
				s.breakLines()
				err := <-errc
				if untilSettled {
					if err != nil && !errors.Is(err, context.Canceled) {
						return fmt.Errorf("%w: %v", errStreamClosed, err)
					}
					return errStreamClosed
				}
				return err
			}
			done, err := s.handle(ctx, st.ev, st.after)
			if err != nil {
				return err
			}
			if done && untilSettled {
				return nil
			}
		}
	}
}

func follow(ctx context.Context, r io.Reader, s *streamer, armed *atomic.Bool, untilSettled bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, errc := startReader(ctx, r, armed)
	return consume(ctx, ch, errc, s, untilSettled)
}

// sendAndFollow abonniert den Ereignisstrom, sendet danach die Nachricht und gibt die Antwort
// aus, bis sie fertig ist. So geht kein Ereignis zwischen Senden und Abonnieren verloren.
func sendAndFollow(ctx context.Context, c *agwclient.Client, chatID, text string, s *streamer, checkRunning bool) (agwclient.SendResult, error) {
	return actAndFollow(ctx, c, chatID, s, checkRunning, func(ctx context.Context) (agwclient.SendResult, error) {
		return c.Send(ctx, chatID, text)
	})
}

// actAndFollow ist sendAndFollow mit beliebiger Aktion (Nachricht oder Slash-Befehl).
func actAndFollow(ctx context.Context, c *agwclient.Client, chatID string, s *streamer, checkRunning bool, act func(context.Context) (agwclient.SendResult, error)) (agwclient.SendResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body, err := c.Events(ctx, chatID)
	if err != nil {
		return agwclient.SendResult{}, err
	}
	defer body.Close()
	var armed atomic.Bool
	ch, errc := startReader(ctx, body, &armed)
	if checkRunning {
		// Läuft pi schon, wird die Nachricht als steer eingereiht; ein neues agent_start kommt
		// dann nicht, und das nächste agent_settled beendet das Warten.
		if det, err := c.Chat(ctx, chatID); err == nil && det.Chat.Running {
			s.sawStart = true
		}
	}
	armed.Store(true)
	res, err := act(ctx)
	if err != nil {
		return res, err
	}
	if res.Queued {
		// Die Nachricht geht mit dem Ende des laufenden Durchgangs; gewartet wird auf den
		// Durchgang danach (dessen agent_start kommt nach der Übergabe).
		s.sawStart = false
		s.note("%s", s.dim("Eingereiht; geht an den Agenten, sobald der laufende Durchgang endet."))
	}
	return res, consume(ctx, ch, errc, s, true)
}

// resumeStep ist ein Schritt beim Fortsetzen eines ruhenden Chats (SSE „resume“, poc/API.md).
type resumeStep struct {
	Phase  string `json:"phase"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Size   *int64 `json:"size"`
	Files  *int   `json:"files"`
	Ms     int64  `json:"ms"`
}

var resumePhaseLabel = map[string]string{
	"acquire":   "Platz aus dem Pool",
	"session":   "Sitzung eingespielt",
	"settings":  "Einstellungen gesetzt",
	"workspace": "Arbeitsbereich",
	"inputs":    "Eingaben bereitgestellt",
}

// resumeLine beschreibt einen abgeschlossenen Schritt; laufende Schritte ergeben "".
func resumeLine(r resumeStep) string {
	secs := deNum(float64(r.Ms)/1000, 1, 1) + " s"
	switch {
	case r.Phase == "ready":
		return "Chat in einer frischen Sandbox fortgesetzt (" + secs + ")."
	case r.Phase == "failed":
		return "Fortsetzen gescheitert: " + r.Detail
	case r.Status == "running":
		return ""
	}
	label := resumePhaseLabel[r.Phase]
	if label == "" {
		label = r.Phase
	}
	var parts []string
	if r.Size != nil && r.Files != nil && (r.Phase == "workspace" || r.Phase == "inputs") {
		parts = append(parts, fmt.Sprintf("%s MB, %d Dateien", deNum(float64(*r.Size)/(1<<20), 1, 1), *r.Files))
	}
	if r.Detail != "" {
		parts = append(parts, r.Detail)
	}
	line := "Fortsetzen: " + label
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, "; ") + ")"
	}
	if r.Status == "warning" || r.Status == "error" {
		line += " – Problem"
	}
	return line + " · " + secs
}

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

// approvalMode determines how the stream processor reacts to pending approvals.
type approvalMode int

const (
	approvalShow   approvalMode = iota // only show (watch)
	approvalAsk                        // ask on the terminal
	approvalAuto                       // approve automatically
	approvalReject                     // reject automatically
)

// streamer turns a chat's SSE events into terminal output: reply text on out,
// everything else (tools, thinking, approvals, errors) on errw.
type streamer struct {
	out, errw io.Writer
	in        *bufio.Reader
	color     bool
	thinking  bool // print thinking deltas
	verbose   bool // also user messages and socket calls (watch)
	calls     bool // show every model call (llm_call) and interventions of the subagent limit (--verbose)
	mode      approvalMode
	chatID    string
	decide    func(ctx context.Context, id string, approve bool) (agwclient.Approval, error)
	// untilCompaction: waiting ends with the first compaction_end after sending
	// (agw chat compact --wait); its result then goes to out.
	untilCompaction bool

	sawStart bool // agent_start seen after sending
	outMid   bool // stdout is in the middle of a line
	errMid   bool // stderr is in the middle of a thinking line
	streamed bool // current reply had text deltas
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

// breakLines closes open lines before a message on stderr follows.
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
	// tool execution
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
	// compaction_start/compaction_end (result in Result)
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

// blocks reads message content, which can be a string or a list of blocks.
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
			sb.WriteString("[image]")
		}
	}
	return sb.String()
}

// resultText: result of a tool execution ({content:[…]}, block list or text).
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

// compactJSON prints JSON on one line, truncated.
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
		s.note("  %s", s.dim("✗ error"))
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
		s.note("  %s", s.dim(fmt.Sprintf("│ … (%d more lines)", n)))
	}
}

// handle processes an event. afterSend says whether it arrived after our own message
// was sent. done is true at the first agent_settled after an agent_start that came
// after sending.
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
			s.note("%s", s.dim(fmt.Sprintf("artifact saved: %s (%d bytes, %s)", a.Name, a.Size, kindLabel(a.Kind))))
		}
	case "error":
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		s.failed = true
		s.note("error: %s", e.Message)
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
			s.note("%s", s.dim(fmt.Sprintf("queued messages handed over (%d).", len(q.IDs))))
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

// socketCall: interventions of the subagent limit also report an error event and therefore
// only appear here with --verbose (or with watch); rejected prompts from extensions
// appear nowhere else and are always shown.
func (s *streamer) socketCall(c agwclient.SocketCall) {
	switch c.Op {
	case "agent_limit":
		if s.calls || s.verbose {
			s.note("%s", s.dim(fmt.Sprintf("· proxy refused a model call (%s)", c.Detail)))
		}
	case "subagent_limit":
		if s.calls || s.verbose {
			s.note("%s", s.dim(fmt.Sprintf("· subagent limit exceeded (%s) – turn %s", c.Detail, orDefault(c.Result, "aborted"))))
		}
	case "extension_ui":
		s.note("%s", s.dim(fmt.Sprintf("· prompt from an extension %s (no input possible): %s", orDefault(c.Result, "rejected"), truncate(c.Detail, 120))))
	default:
		if s.verbose {
			s.note("%s", s.dim(fmt.Sprintf("· Socket %s %s %s → %s", c.Via, c.Op, truncate(c.Detail, 80), truncate(c.Result, 80))))
		}
	}
}

// subagent prints an entry from a subagent session, indented.
func (s *streamer) subagent(e agwclient.SubagentEntry) {
	prefix := "  ↳ " + orDefault(e.Agent, "Subagent") + ": "
	p := e.Payload
	switch e.Kind {
	case "task":
		s.note("%s", prefix+"task: "+truncate(oneLine(p.Text), 120))
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
				s.note("error: %s", orDefault(p.Message.ErrorMessage, "The reply broke off with an error."))
			case "aborted":
				s.note("Reply aborted.")
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
		msg := "summarising the context"
		if p.Reason != "" {
			msg += " (" + compactionReason(p.Reason) + ")"
		}
		s.note("%s", s.dim(msg+" …"))
	case "compaction_end":
		return s.compactionEnd(p, afterSend)
	case "auto_retry_start":
		msg := "retry"
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

// compactionResult: the fields of compaction_end.result needed for the output.
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
		msg := "compaction aborted"
		if !p.Aborted {
			msg = "compaction without a result"
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
			s.note("approval %s (%s): %s", a.ID, a.Name, approvalState(a.State))
		}
		return nil
	}
	if s.handled[a.ID] {
		return nil
	}
	s.handled[a.ID] = true
	internet := a.Kind == "internet_access"
	desc := fmt.Sprintf("artifact %s (%d bytes)", a.Name, a.Size)
	question := "Approve " + desc + "? [y/n] "
	if internet {
		desc = "internet access"
		question = "Allow internet access? [y/n] "
	}
	if a.Kind == "platform_write" {
		desc = "platform call " + a.Name
		question = "Run " + desc + "? [y/n] "
	}
	request := "agent requests internet access: " + orDefault(a.Name, "(no reason given)")
	var approve bool
	switch s.mode {
	case approvalShow:
		what := desc
		if internet {
			what = request
		}
		s.note("pending approval %s: %s via %s – agw approve %s | agw reject %s", a.ID, what, a.Via, a.ID, a.ID)
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
			s.note("%s", s.dim("preview:"))
			lines := strings.Split(strings.TrimRight(a.Preview, "\n"), "\n")
			for i, l := range lines {
				// The console shows a platform call in full: what is approved is what can be seen (Review W1).
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
			s.note("no input – approval stays pending: agw approve %s | agw reject %s", a.ID, a.ID)
			return nil
		}
	}
	if s.decide == nil {
		return errors.New("no decision function set")
	}
	res, err := s.decide(ctx, a.ID, approve)
	if err != nil {
		s.failed = true
		s.note("error deciding on %s: %v", a.ID, err)
		return nil
	}
	word := "rejected"
	if approve {
		word = "approved"
	}
	if s.mode == approvalAuto || s.mode == approvalReject {
		s.note("%s %s automatically.", desc, word)
	} else {
		s.note("%s %s.", desc, word)
	}
	if res.State != "" && res.State != "pending" {
		s.handled[a.ID+"/"+res.State] = true
	}
	return nil
}

// ask asks on stderr and reads the answer from in. ok is false at the end of input.
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
		io.WriteString(s.errw, "Please enter y or n.\n")
	}
}

type stamped struct {
	ev    agwclient.Event
	after bool
}

// startReader reads the SSE stream concurrently. Each event is stamped on arrival with the
// state of armed (nil = always after sending).
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

var errStreamClosed = errors.New("connection to the orchestrator broke off before the reply was complete")

// consume processes events until (with untilSettled) the reply is complete, the stream
// ends or ctx is cancelled.
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

// sendAndFollow subscribes to the event stream, then sends the message and prints the reply
// until it is complete. That way no event is lost between sending and subscribing.
func sendAndFollow(ctx context.Context, c *agwclient.Client, chatID, text string, s *streamer, checkRunning bool) (agwclient.SendResult, error) {
	return actAndFollow(ctx, c, chatID, s, checkRunning, func(ctx context.Context) (agwclient.SendResult, error) {
		return c.Send(ctx, chatID, text)
	})
}

// actAndFollow is sendAndFollow with any action (message or slash command).
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
		// If pi is already running, the message is enqueued as steer; no new agent_start
		// comes then, and the next agent_settled ends the waiting.
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
		// The message goes with the end of the running turn; we wait for the turn
		// after that (its agent_start comes after the handover).
		s.sawStart = false
		s.note("%s", s.dim("Queued; goes to the agent as soon as the running turn ends."))
	}
	return res, consume(ctx, ch, errc, s, true)
}

// resumeStep is a step when resuming an idle chat (SSE "resume", API.md).
type resumeStep struct {
	Phase  string `json:"phase"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Size   *int64 `json:"size"`
	Files  *int   `json:"files"`
	Ms     int64  `json:"ms"`
}

var resumePhaseLabel = map[string]string{
	"acquire":   "slot from the pool",
	"session":   "session restored",
	"settings":  "settings applied",
	"workspace": "workspace",
	"inputs":    "inputs provided",
}

// resumeLine describes a completed step; running steps yield "".
func resumeLine(r resumeStep) string {
	secs := fmtNum(float64(r.Ms)/1000, 1, 1) + " s"
	switch {
	case r.Phase == "ready":
		return "chat resumed in a fresh sandbox (" + secs + ")."
	case r.Phase == "failed":
		return "resume failed: " + r.Detail
	case r.Status == "running":
		return ""
	}
	label := resumePhaseLabel[r.Phase]
	if label == "" {
		label = r.Phase
	}
	var parts []string
	if r.Size != nil && r.Files != nil && (r.Phase == "workspace" || r.Phase == "inputs") {
		parts = append(parts, fmt.Sprintf("%s MB, %d files", fmtNum(float64(*r.Size)/(1<<20), 1, 1), *r.Files))
	}
	if r.Detail != "" {
		parts = append(parts, r.Detail)
	}
	line := "resume: " + label
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, "; ") + ")"
	}
	if r.Status == "warning" || r.Status == "error" {
		line += " – problem"
	}
	return line + " · " + secs
}

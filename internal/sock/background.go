package sock

// Endpoints of the background tasks at pi's socket. The start comes from the tool bash
// (run_in_background), retrieval and stop from the bridge's tools bg_output and bg_stop. Every
// call is in tool_executions (tool bash with operation bg_start, or bg_output, bg_stop),
// so that the reconciliation with the proxy lists it as recorded.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"agw/internal/bgtask"
	"agw/internal/execproto"
	"agw/internal/store"
)

// Operations in the log (tool_executions.op).
const (
	OpBgStart  = "bg_start"
	OpBgOutput = "bg_output"
	OpBgStop   = "bg_stop"
)

type bgRequest struct {
	ToolCallID  string            `json:"toolCallId"`
	Tool        string            `json:"tool"`
	SessionFile string            `json:"sessionFile"`
	Req         execproto.Request `json:"req"`       // start only
	ID          string            `json:"id"`        // output, stop
	TailLines   int               `json:"tailLines"` // output
}

// BgResponse is the response of the endpoints: the state of the task, for output the tail of the
// output (up to bgtask.TailBytes), on an error only Error (English, goes to the model).
type BgResponse struct {
	Task   *store.BackgroundTask `json:"task,omitempty"`
	Output string                `json:"output,omitempty"`
	Max    int                   `json:"max,omitempty"`
	Error  string                `json:"error,omitempty"`
}

// decodeBg reads and checks a request; on an error the response has been written.
func (th *toolHandler) decodeBg(w http.ResponseWriter, r *http.Request, tool string) (bgRequest, string, func(), bool) {
	release := func() {}
	if th.bg == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "background tasks not available"})
		return bgRequest{}, "", release, false
	}
	rel, err := th.acquire(r.Context(), 0)
	if err != nil {
		return bgRequest{}, "", release, false
	}
	var br bgRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, execproto.MaxCommand+64<<10)).Decode(&br); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable request"})
		return br, "", rel, false
	}
	chat, err := th.chat("tool", "tool", tool)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "slot not assigned"})
		return br, "", rel, false
	}
	if br.Tool != tool {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint only for " + tool})
		return br, "", rel, false
	}
	if !validToolCallID(br.ToolCallID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid toolCallId"})
		return br, "", rel, false
	}
	return br, chat, rel, true
}

func (th *toolHandler) recordBg(chat string, br bgRequest, op string, args any, out string, bt *store.BackgroundTask, errText string, start time.Time) {
	a, _ := json.Marshal(args)
	d := newDigest(excerptBytes)
	d.Write([]byte(out))
	e := store.ToolExecution{ChatID: chat, SlotID: th.slot, Session: SessionKey(br.SessionFile), ToolCallID: br.ToolCallID,
		Tool: br.Tool, Op: op, Args: a, Error: noNUL(errText), OutputExcerpt: d.Excerpt(), OutputSHA256: d.Sum(),
		OutputBytes: d.n, StartedAt: start, DurationMs: time.Since(start).Milliseconds()}
	if bt != nil && bt.ExitCode != nil && op != OpBgStart {
		e.ExitCode = bt.ExitCode
	}
	th.rec.RecordToolExecution(e)
}

func (th *toolHandler) bgStart(w http.ResponseWriter, r *http.Request) {
	br, chat, release, ok := th.decodeBg(w, r, "bash")
	defer release()
	if !ok {
		return
	}
	req := br.Req
	req.Op, req.Spill = execproto.OpBg, execproto.BgLogPath(1) // path only for validation; the registry sets it
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	args := map[string]any{}
	_ = json.Unmarshal(summarize(req), &args)
	args["run_in_background"] = true
	start := time.Now()
	bt, err := th.bg.Start(r.Context(), bgtask.StartParams{ChatID: chat, Session: SessionKey(br.SessionFile), ToolCallID: br.ToolCallID,
		Command: req.Command, Cwd: req.Cwd, Env: withToolCallEnv(req.Env, br.ToolCallID, SessionKey(br.SessionFile)), Timeout: req.Timeout})
	res := BgResponse{Max: th.bg.Max()}
	if err != nil {
		res.Error = err.Error()
	}
	if bt.Seq > 0 {
		res.Task = &bt
		args["id"] = bt.ID
	}
	out, _ := json.Marshal(res)
	th.recordBg(chat, br, OpBgStart, args, string(out), res.Task, res.Error, start)
	writeJSON(w, http.StatusOK, res)
}

func (th *toolHandler) bgOutput(w http.ResponseWriter, r *http.Request) {
	br, chat, release, ok := th.decodeBg(w, r, BgOutputTool)
	defer release()
	if !ok {
		return
	}
	start := time.Now()
	bt, out, err := th.bg.Output(r.Context(), chat, br.ID)
	res := BgResponse{Output: out}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Task = &bt
	}
	th.recordBg(chat, br, OpBgOutput, map[string]any{"id": br.ID, "tail_lines": br.TailLines}, out, res.Task, res.Error, start)
	writeJSON(w, http.StatusOK, res)
}

func (th *toolHandler) bgStop(w http.ResponseWriter, r *http.Request) {
	br, chat, release, ok := th.decodeBg(w, r, BgStopTool)
	defer release()
	if !ok {
		return
	}
	start := time.Now()
	bt, err := th.bg.Stop(r.Context(), chat, br.ID, "agent")
	res := BgResponse{}
	if err != nil {
		res.Error = err.Error()
		if !errors.Is(err, bgtask.ErrUnknown) {
			res.Error = "bg_stop: " + err.Error()
		}
	} else {
		res.Task = &bt
	}
	out, _ := json.Marshal(res)
	th.recordBg(chat, br, OpBgStop, map[string]any{"id": br.ID}, string(out), res.Task, res.Error, start)
	writeJSON(w, http.StatusOK, res)
}

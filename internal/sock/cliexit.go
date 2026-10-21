// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package sock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agw/internal/execproto"
)

// cliExit: POST /cli/exit takes the report of a gateway CLI that ends with a code other than 0
// ({"cli":"agw-internet","exit":3}, #202). It is attached to the running bash call named by X-Agw-Tool-Call
// and ends up in the final frame, so that a pipe (`| tail`) cannot hide the failure. A report can only make a
// call fail, never succeed, so the agent-controlled header is no risk. Always 204, even for a dropped report.
func (h *handler) cliExit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CLI  string `json:"cli"`
		Exit int    `json:"exit"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10+1))
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	switch req.CLI {
	case "agw-artifact", "agw-internet", "agw-platform":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown cli"})
		return
	}
	if req.Exit < 1 || req.Exit > 255 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exit must be 1..255"})
		return
	}
	chat, err := h.chat("cli", "cli_exit", "")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not assigned"})
		return
	}
	id := r.Header.Get("X-Agw-Tool-Call")
	noted := validToolCallID(id) && h.fg != nil && h.fg.NoteCLIExit(chat, id, execproto.CLIExit{CLI: req.CLI, Exit: req.Exit})
	result := "ignored: no running command"
	if noted {
		result = "noted"
	}
	h.logCall(h.callerCtx(r), chat, "cli", "cli_exit", fmt.Sprintf("%s %d", req.CLI, req.Exit), result)
	w.WriteHeader(http.StatusNoContent)
}

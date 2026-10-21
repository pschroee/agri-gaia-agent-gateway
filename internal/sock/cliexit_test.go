// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"agw/internal/execproto"
)

func cliExitHandler(chat string) (http.Handler, *fakeBackend, *Foreground) {
	b := &fakeBackend{chat: chat}
	fg := NewForeground()
	return NewHandlerRunFg("p-test", b, 1<<20, nil, fg), b, fg
}

func postCLIExit(h http.Handler, call, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://agw/cli/exit", strings.NewReader(body))
	if call != "" {
		r.Header.Set("X-Agw-Tool-Call", call)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCLIExitNotedForRunningCall(t *testing.T) {
	h, b, fg := cliExitHandler("chat-1")
	op := fg.add("tc1", "chat-1", func() {})
	if w := postCLIExit(h, "tc1", `{"cli":"agw-internet","exit":3}`); w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
	want := []execproto.CLIExit{{CLI: "agw-internet", Exit: 3}}
	if got := op.cliExits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("exits: %+v", got)
	}
	if len(b.calls) != 1 || b.calls[0] != "cli:cli_exit:noted" || b.details[0] != "agw-internet 3" {
		t.Fatalf("log: %v %v", b.calls, b.details)
	}
}

func TestCLIExitIgnoredWithoutRunningCall(t *testing.T) {
	h, b, fg := cliExitHandler("chat-1")
	if w := postCLIExit(h, "tc1", `{"cli":"agw-platform","exit":1}`); w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
	if len(fg.ops) != 0 || len(b.calls) != 1 || b.calls[0] != "cli:cli_exit:ignored: no running command" {
		t.Fatalf("log: %v", b.calls)
	}
	// no header at all
	if w := postCLIExit(h, "", `{"cli":"agw-platform","exit":1}`); w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestCLIExitOtherChat(t *testing.T) {
	h, b, fg := cliExitHandler("chat-1")
	op := fg.add("tc1", "chat-2", func() {})
	if w := postCLIExit(h, "tc1", `{"cli":"agw-internet","exit":2}`); w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
	if len(op.cliExits()) != 0 || !strings.HasSuffix(b.calls[0], "ignored: no running command") {
		t.Fatalf("exits %+v, log %v", op.cliExits(), b.calls)
	}
}

func TestCLIExitValidation(t *testing.T) {
	h, _, fg := cliExitHandler("chat-1")
	op := fg.add("tc1", "chat-1", func() {})
	for name, body := range map[string]string{
		"unknown cli": `{"cli":"rm","exit":1}`,
		"exit 0":      `{"cli":"agw-internet","exit":0}`,
		"exit 300":    `{"cli":"agw-internet","exit":300}`,
		"not json":    `nope`,
		"too large":   `{"cli":"agw-internet","exit":1,"pad":"` + strings.Repeat("x", 2000) + `"}`,
	} {
		if w := postCLIExit(h, "tc1", body); w.Code != 400 {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
	if len(op.cliExits()) != 0 {
		t.Fatalf("stored: %+v", op.cliExits())
	}
}

func TestCLIExitCap(t *testing.T) {
	h, _, fg := cliExitHandler("chat-1")
	op := fg.add("tc1", "chat-1", func() {})
	for i := 0; i < 9; i++ {
		postCLIExit(h, "tc1", `{"cli":"agw-artifact","exit":1}`)
	}
	if n := len(op.cliExits()); n != 8 {
		t.Fatalf("stored %d, want 8", n)
	}
}

func TestCLIExitNotOnPiSocket(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	h := NewPiHandlerFg("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}, nil, NewForeground())
	w := postCLIExit(h, "tc1", `{"cli":"agw-internet","exit":3}`)
	if w.Code != 404 && w.Code != 405 {
		t.Fatalf("status %d", w.Code)
	}
}

// reporter runs a bash call that reports CLI exits while it runs (as agw-internet would).
type cliReporter struct {
	fg    *Foreground
	exits []execproto.CLIExit
}

func (c *cliReporter) Run(_ context.Context, req execproto.Request, _ func([]byte)) (execproto.Frame, error) {
	for _, e := range c.exits {
		c.fg.NoteCLIExit("chat-1", "call_cli", e)
	}
	code := 0
	return execproto.Frame{Done: true, Exit: &code}, nil
}

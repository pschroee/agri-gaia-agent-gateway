package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// fakePi simulates the other side: reads commands from cmdR and writes
// responses/events to outW.
type fakePi struct {
	cmds chan map[string]any
	outW *io.PipeWriter
}

func newPair(t *testing.T) (*Client, *fakePi) {
	t.Helper()
	cmdR, cmdW := io.Pipe()
	outR, outW := io.Pipe()
	c := New(cmdW, outR)
	f := &fakePi{cmds: make(chan map[string]any, 16), outW: outW}
	go func() {
		sc := bufio.NewScanner(cmdR)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("command not JSON: %q", sc.Text())
				return
			}
			f.cmds <- m
		}
	}()
	t.Cleanup(func() { outW.Close(); cmdW.Close(); c.Close() })
	return c, f
}

func (f *fakePi) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(f.outW, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func TestCallMatchesResponseByID(t *testing.T) {
	c, f := newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan Response, 1)
	go func() {
		r, err := c.Call(ctx, map[string]any{"type": "get_state"})
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	cmd := <-f.cmds
	if cmd["type"] != "get_state" {
		t.Fatalf("wrong command: %v", cmd)
	}
	id, _ := cmd["id"].(string)
	if id == "" {
		t.Fatal("command without id")
	}
	// An event in between must not disturb the response.
	f.send(t, `{"type":"agent_start"}`)
	f.send(t, `{"id":"`+id+`","type":"response","command":"get_state","success":true,"data":{"sessionFile":"/agent/sessions/a.jsonl"}}`)
	r := <-done
	if !r.Success || r.Command != "get_state" {
		t.Fatalf("unexpected response: %+v", r)
	}
	var data struct{ SessionFile string }
	if err := json.Unmarshal(r.Data, &data); err != nil || data.SessionFile != "/agent/sessions/a.jsonl" {
		t.Fatalf("wrong data: %s", r.Data)
	}
	ev := <-c.Events()
	if ev.Type != "agent_start" {
		t.Fatalf("wrong event: %+v", ev)
	}
}

func TestCallReturnsErrorOnFailure(t *testing.T) {
	c, f := newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, map[string]any{"type": "prompt", "message": "x"})
		errc <- err
	}()
	cmd := <-f.cmds
	f.send(t, `{"id":"`+cmd["id"].(string)+`","type":"response","command":"prompt","success":false,"error":"busy"}`)
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("expected error, got %v", err)
	}
}

// U+2028 and U+2029 are valid in JSON strings and must not count as
// line ends (docs/rpc.md, framing).
func TestFramingOnlySplitsOnLF(t *testing.T) {
	c, f := newPair(t)
	f.send(t, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"a`+" "+`b`+" "+`c"}}`)
	ev := <-c.Events()
	var p struct {
		AssistantMessageEvent struct{ Delta string } `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(ev.Raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.AssistantMessageEvent.Delta != "a b c" {
		t.Fatalf("delta destroyed: %q", p.AssistantMessageEvent.Delta)
	}
}

func TestFramingStripsCR(t *testing.T) {
	c, f := newPair(t)
	f.send(t, "{\"type\":\"agent_settled\"}\r")
	if ev := <-c.Events(); ev.Type != "agent_settled" {
		t.Fatalf("CR not removed: %+v", ev)
	}
}

func TestLongLinesAreDelivered(t *testing.T) {
	c, f := newPair(t)
	big := strings.Repeat("x", 3<<20) // 3 MiB, far above bufio.Scanner's 64 KiB
	go io.WriteString(f.outW, `{"type":"tool_execution_end","result":{"content":[{"type":"text","text":"`+big+`"}]}}`+"\n")
	select {
	case ev := <-c.Events():
		if ev.Type != "tool_execution_end" || len(ev.Raw) < len(big) {
			t.Fatalf("long line incomplete: %d bytes", len(ev.Raw))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long line did not arrive")
	}
}

func TestOversizedLineIsReportedNotDropped(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	outR, outW := io.Pipe()
	defer cmdR.Close()
	c := NewWithLimit(cmdW, outR, 1024)
	defer c.Close()
	go func() {
		io.WriteString(outW, `{"type":"x","pad":"`+strings.Repeat("y", 4096)+`"}`+"\n")
		io.WriteString(outW, `{"type":"agent_settled"}`+"\n")
		outW.Close()
	}()
	ev := <-c.Events()
	if ev.Type != TypeOversized {
		t.Fatalf("oversized line not reported: %+v", ev.Type)
	}
	if ev := <-c.Events(); ev.Type != "agent_settled" {
		t.Fatalf("stream broken after oversized line: %+v", ev.Type)
	}
}

func TestNonJSONLineIsReported(t *testing.T) {
	c, f := newPair(t)
	f.send(t, "Warning: something on stdout")
	if ev := <-c.Events(); ev.Type != TypeInvalid {
		t.Fatalf("invalid line not reported: %+v", ev)
	}
}

func TestCallFailsWhenStreamEnds(t *testing.T) {
	c, f := newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, map[string]any{"type": "get_state"})
		errc <- err
	}()
	<-f.cmds
	f.outW.Close()
	if err := <-errc; err == nil {
		t.Fatal("expected error when pi ends")
	}
	// The event channel is closed.
	for range c.Events() {
	}
}

// H1: if nobody reads the events (e.g. because the reader is itself waiting for a
// response), a flood of events must not block the response.
// Excess events are discarded and reported as TypeOverflow.
func TestResponseDeliveredDespiteEventFlood(t *testing.T) {
	c, f := newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, map[string]any{"type": "get_state"})
		errc <- err
	}()
	cmd := <-f.cmds
	go func() {
		for i := 0; i < 3000; i++ {
			io.WriteString(f.outW, `{"type":"message_update"}`+"\n")
		}
		io.WriteString(f.outW, `{"id":"`+cmd["id"].(string)+`","type":"response","command":"get_state","success":true}`+"\n")
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("response blocked by unread events")
	}
	sawOverflow := false
	for i := 0; i < 5000; i++ {
		select {
		case ev := <-c.Events():
			if ev.Type == TypeOverflow {
				sawOverflow = true
			}
		default:
			i = 5000
		}
	}
	io.WriteString(f.outW, `{"type":"agent_settled"}`+"\n")
	deadline := time.After(2 * time.Second)
	for !sawOverflow {
		select {
		case ev := <-c.Events():
			if ev.Type == TypeOverflow {
				sawOverflow = true
			}
		case <-deadline:
			t.Fatal("loss of events not reported")
		}
	}
}

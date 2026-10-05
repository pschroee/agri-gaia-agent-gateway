// Package rpc spricht das RPC-Protokoll von pi (docs/rpc.md): Befehle als
// JSON-Zeilen auf stdin, Antworten und Ereignisse als JSON-Zeilen auf stdout.
// Getrennt wird nur an '\n'; U+2028/U+2029 sind gültiger Teil von JSON-Strings.
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// DefaultMaxLine begrenzt eine Zeile. pi liest Daten, die der Agent erzeugt;
// eine Grenze schützt den Orchestrator. Längere Zeilen werden nicht still
// verworfen, sondern als Ereignis TypeOversized gemeldet (docs/entwurf.md).
const DefaultMaxLine = 32 << 20

const (
	TypeOversized = "agw_oversized_line"
	TypeInvalid   = "agw_invalid_line"
	// TypeOverflow meldet verworfene Ereignisse (Raw: Anzahl). Der Lesefaden
	// blockiert nie an vollen Ereignissen, damit Antworten immer zugestellt werden.
	TypeOverflow = "agw_events_dropped"
)

type Response struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// Event ist ein pi-Ereignis. Raw enthält die Zeile unverändert.
type Event struct {
	Type string
	Raw  json.RawMessage
}

type Client struct {
	w       io.Writer
	wmu     sync.Mutex
	next    atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan Response
	closed  bool
	events  chan Event
	done    chan struct{}
	err     error
	dropped int // nur im Lesefaden benutzt
}

func New(w io.Writer, r io.Reader) *Client { return NewWithLimit(w, r, DefaultMaxLine) }

func NewWithLimit(w io.Writer, r io.Reader, maxLine int) *Client {
	c := &Client{
		w:       w,
		pending: map[string]chan Response{},
		events:  make(chan Event, 1024),
		done:    make(chan struct{}),
	}
	go c.readLoop(r, maxLine)
	return c
}

// Events liefert alle Zeilen, die keine Antwort sind. Der Kanal wird
// geschlossen, wenn pi endet.
func (c *Client) Events() <-chan Event { return c.events }

// Done wird geschlossen, wenn der Strom endet.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Err() error {
	<-c.done
	return c.err
}

func (c *Client) readLoop(r io.Reader, maxLine int) {
	defer close(c.done)
	defer close(c.events)
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	oversized := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && !oversized {
			if len(buf)+len(chunk) > maxLine {
				oversized = true
				buf = buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			c.fail(err)
			return
		}
		line := bytes.TrimRight(buf, "\r\n")
		switch {
		case oversized:
			c.emit(Event{Type: TypeOversized})
		case len(line) > 0:
			c.dispatch(append([]byte(nil), line...))
		}
		buf, oversized = buf[:0], false
		if err == io.EOF {
			c.fail(io.EOF)
			return
		}
	}
}

func (c *Client) dispatch(line []byte) {
	var head struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil || head.Type == "" {
		c.emit(Event{Type: TypeInvalid, Raw: mustJSONString(line)})
		return
	}
	if head.Type == "response" && head.ID != "" {
		var resp Response
		_ = json.Unmarshal(line, &resp)
		c.mu.Lock()
		ch := c.pending[head.ID]
		delete(c.pending, head.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
			return
		}
	}
	c.emit(Event{Type: head.Type, Raw: line})
}

// emit stellt ein Ereignis zu, ohne zu blockieren. Ist der Puffer voll, wird
// es verworfen; sobald wieder Platz ist, folgt ein TypeOverflow mit der Anzahl.
func (c *Client) emit(ev Event) {
	if c.dropped > 0 {
		select {
		case c.events <- Event{Type: TypeOverflow, Raw: json.RawMessage(strconv.Itoa(c.dropped))}:
			c.dropped = 0
		default:
			c.dropped++
			return
		}
	}
	select {
	case c.events <- ev:
	default:
		c.dropped++
	}
}

func mustJSONString(b []byte) json.RawMessage {
	out, _ := json.Marshal(string(b))
	return out
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

var ErrClosed = errors.New("pi-RPC-Strom beendet")

// Call sendet einen Befehl und wartet auf die zugehörige Antwort.
func (c *Client) Call(ctx context.Context, cmd map[string]any) (Response, error) {
	id := "agw-" + strconv.FormatUint(c.next.Add(1), 10)
	msg := make(map[string]any, len(cmd)+1)
	for k, v := range cmd {
		msg[k] = v
	}
	msg["id"] = id
	line, err := json.Marshal(msg)
	if err != nil {
		return Response{}, err
	}
	ch := make(chan Response, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Response{}, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	c.wmu.Lock()
	_, err = c.w.Write(append(line, '\n'))
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return Response{}, fmt.Errorf("Befehl %v senden: %w", cmd["type"], err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return Response{}, ErrClosed
		}
		if !resp.Success {
			return resp, fmt.Errorf("pi lehnt %s ab: %s", resp.Command, resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return Response{}, ctx.Err()
	}
}

// Notify schreibt einen Befehl, ohne auf eine Antwort zu warten
// (etwa extension_ui_response).
func (c *Client) Notify(cmd map[string]any) error {
	line, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(line, '\n'))
	return err
}

// Close schließt den Schreibkanal, sofern er ein io.Closer ist.
func (c *Client) Close() error {
	if cl, ok := c.w.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

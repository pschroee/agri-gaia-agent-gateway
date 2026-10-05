package agwclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Event is an event from GET /api/chats/{id}/events.
type Event struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// PiType returns the pi event type (data.type) for kind "pi", otherwise "".
func (e Event) PiType() string {
	if e.Kind != "pi" {
		return ""
	}
	var t struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(e.Data, &t)
	return t.Type
}

// ReadEvents reads an SSE stream and calls fn for every event. Comments (": ping")
// and unknown fields are skipped, as are events with invalid JSON. If fn returns an
// error, ReadEvents aborts with it. At the end of the stream the result is nil.
func ReadEvents(r io.Reader, fn func(Event) error) error {
	br := bufio.NewReader(r)
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil || ev.Kind == "" {
			return nil
		}
		return fn(ev)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		eof := err != nil
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if e := dispatch(); e != nil {
				return e
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line, "data:")
			data = append(data, strings.TrimPrefix(v, " "))
		}
		if eof {
			return dispatch()
		}
	}
}

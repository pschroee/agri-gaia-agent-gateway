package agwclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Event ist ein Ereignis aus GET /api/chats/{id}/events.
type Event struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// PiType liefert bei kind "pi" den pi-Ereignistyp (data.type), sonst "".
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

// ReadEvents liest einen SSE-Strom und ruft fn für jedes Ereignis auf. Kommentare (": ping")
// und fremde Felder werden übergangen, Ereignisse mit ungültigem JSON ebenfalls. Gibt fn einen
// Fehler zurück, bricht ReadEvents damit ab. Am Stromende ist das Ergebnis nil.
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

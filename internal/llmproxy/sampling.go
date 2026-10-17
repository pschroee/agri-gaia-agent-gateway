// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

// setSampling replaces or adds the top-level fields "temperature" and "seed" of a request body (X3.6). A nil
// value leaves the field as the sandbox sent it; a field the model does not take is never touched. A field that
// appears more than once on the top level is refused: encoding/json matches keys loosely and the provider reads
// a different one than a lenient reader would, so the recorded "sent" value could be wrong.
func setSampling(body []byte, temperature *float64, seed *int64) ([]byte, error) {
	if temperature == nil && seed == nil {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("request is not a JSON object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	seenTemp, seenSeed := false, false
	wroteTemp, wroteSeed := false, false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("request is not a JSON object")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		switch key {
		case "temperature":
			if temperature != nil {
				if seenTemp {
					return nil, errors.New("field temperature present more than once")
				}
				seenTemp = true
				val = []byte(strconv.FormatFloat(*temperature, 'g', -1, 64))
				wroteTemp = true
			}
		case "seed":
			if seed != nil {
				if seenSeed {
					return nil, errors.New("field seed present more than once")
				}
				seenSeed = true
				val = []byte(strconv.FormatInt(*seed, 10))
				wroteSeed = true
			}
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		kb, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out.Write(kb)
		out.WriteByte(':')
		out.Write(val)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("request is not a JSON object")
	}
	add := func(name string, raw []byte) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.WriteString(strconv.Quote(name))
		out.WriteByte(':')
		out.Write(raw)
	}
	if temperature != nil && !wroteTemp {
		add("temperature", []byte(strconv.FormatFloat(*temperature, 'g', -1, 64)))
	}
	if seed != nil && !wroteSeed {
		add("seed", []byte(strconv.FormatInt(*seed, 10)))
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

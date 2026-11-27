// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// setSampling replaces or adds the top-level fields "temperature" and "seed" of a request body (X3.6). A nil
// value leaves the field as the sandbox sent it; a field the model does not take is never touched.
func setSampling(body []byte, temperature *float64, seed *int64) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if temperature != nil {
		fields["temperature"] = json.RawMessage(strconv.FormatFloat(*temperature, 'g', -1, 64))
	}
	if seed != nil {
		fields["seed"] = json.RawMessage(strconv.FormatInt(*seed, 10))
	}
	return setFields(body, fields)
}

// setFields replaces or adds top-level fields of a request body: every key of fields ends up with exactly the
// given value, whatever the sandbox sent (the proxy owns these fields). Other fields stay as they were. A field
// that appears more than once on the top level is refused: encoding/json matches keys loosely and the provider
// reads a different one than a lenient reader would, so the recorded "sent" value could be wrong. New fields
// are appended in sorted order, so the result does not depend on map order. Without fields the body is returned
// unchanged.
func setFields(body []byte, fields map[string]json.RawMessage) ([]byte, error) {
	if len(fields) == 0 {
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
	written := map[string]bool{}
	write := func(key string, val []byte) error {
		if !first {
			out.WriteByte(',')
		}
		first = false
		kb, err := json.Marshal(key)
		if err != nil {
			return err
		}
		out.Write(kb)
		out.WriteByte(':')
		out.Write(val)
		return nil
	}
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
		if repl, own := fields[key]; own {
			if written[key] {
				return nil, fmt.Errorf("field %s present more than once", key)
			}
			written[key] = true
			val = repl
		}
		if err := write(key, val); err != nil {
			return nil, err
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("request is not a JSON object")
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		if !written[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		if err := write(k, fields[k]); err != nil {
			return nil, err
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

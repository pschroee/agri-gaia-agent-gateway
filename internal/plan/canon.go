// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxDepth limits the nesting of a body that is canonicalised (Canonical).
const maxDepth = 64

// Canonical returns the body in a canonical form: object keys sorted bytewise, no white space,
// numbers kept as written (2.50 stays 2.50), strings without HTML escaping. It reads the JSON
// token by token: encoding/json would match keys case-insensitively and let the last of two
// duplicate keys win, so the agent and the platform could read the same body differently.
// Duplicate keys, invalid UTF-8, data after the value and nesting deeper than 64 are errors.
// Empty input gives nil.
func Canonical(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("body is not valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var buf bytes.Buffer
	if err := canonValue(d, &buf, 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON value")
	}
	return buf.Bytes(), nil
}

func canonValue(d *json.Decoder, buf *bytes.Buffer, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("nested deeper than %d levels", maxDepth)
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			type member struct {
				key string
				val []byte
			}
			var members []member
			seen := map[string]bool{}
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := kt.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if seen[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				seen[k] = true
				var vb bytes.Buffer
				if err := canonValue(d, &vb, depth+1); err != nil {
					return err
				}
				members = append(members, member{k, vb.Bytes()})
			}
			if _, err := d.Token(); err != nil { // closing brace
				return err
			}
			sort.Slice(members, func(i, j int) bool { return members[i].key < members[j].key })
			buf.WriteByte('{')
			for i, m := range members {
				if i > 0 {
					buf.WriteByte(',')
				}
				writeString(buf, m.key)
				buf.WriteByte(':')
				buf.Write(m.val)
			}
			buf.WriteByte('}')
		case '[':
			buf.WriteByte('[')
			first := true
			for d.More() {
				if !first {
					buf.WriteByte(',')
				}
				first = false
				if err := canonValue(d, buf, depth+1); err != nil {
					return err
				}
			}
			if _, err := d.Token(); err != nil { // closing bracket
				return err
			}
			buf.WriteByte(']')
		default:
			return fmt.Errorf("unexpected %v", t)
		}
	case string:
		writeString(buf, t)
	case json.Number:
		buf.WriteString(t.String())
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	default:
		return fmt.Errorf("unexpected token %v", tok)
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	buf.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

// QueryJSON is the query as stored and compared: keys sorted, no HTML escaping. Empty is "".
func QueryJSON(q map[string]string) string {
	if len(q) == 0 {
		return ""
	}
	return encodeNoEscape(q)
}

// FormJSON is the form as stored and compared: keys sorted, no HTML escaping. Empty is "".
func FormJSON(f map[string][]string) string {
	if len(f) == 0 {
		return ""
	}
	return encodeNoEscape(f)
}

func encodeNoEscape(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// SHA256Hex is the hex SHA-256 of s.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashOrEmpty is SHA256Hex, but "" for "" (no body, no form).
func hashOrEmpty(s string) string {
	if s == "" {
		return ""
	}
	return SHA256Hex(s)
}

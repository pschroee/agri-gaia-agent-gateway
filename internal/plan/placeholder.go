// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Placeholders name the id of an object an earlier step created: "{{stepN.id}}". They may stand
// only as a whole value (a JSON string, a whole path segment, a whole query value). While the
// step is built they are replaced by a sentinel number, which tool.Build accepts as an id and
// which is turned back into the placeholder in the stored step (restorePath, restoreQuery,
// restoreJSON). The sentinel is 98765432 followed by four digits.

var (
	wholeRe    = regexp.MustCompile(`^\{\{step([1-9][0-9]?)\.id\}\}$`)
	canonRefRe = regexp.MustCompile(`"\{\{step([1-9][0-9]?)\.id\}\}"`)
	anyRefRe   = regexp.MustCompile(`\{\{step([1-9][0-9]?)\.id\}\}`)
	sentinelRe = regexp.MustCompile(`98765432[0-9]{4}`)
	sentinelOK = regexp.MustCompile(`^98765432[0-9]{4}$`)
	idRe       = regexp.MustCompile(`^[A-Za-z0-9_\-.:~]+$`)
	digitsRe   = regexp.MustCompile(`^[0-9]+$`)
)

var errPlaceholderText = errors.New("placeholder inside a text (a {{stepN.id}} may only stand alone as a whole value)")

// errPlaceholderAloneInField is the refusal of a placeholder that stands alone as the value of a form field other than
// base_container_id of a training search (#254): the cause is the field, not a text around the placeholder.
var errPlaceholderAloneInField = errors.New("a {{stepN.id}} in a form field is only allowed as base_container_id of a training search step; give the id itself or make the step a call without files")

// sentinel is the number that stands for placeholder N while the step is built.
func sentinel(n int) string { return fmt.Sprintf("98765432%04d", n) }

// wholePlaceholder returns N if s is exactly a placeholder.
func wholePlaceholder(s string) (int, bool) {
	m := wholeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

// sentinelJSON replaces every JSON string that is exactly a placeholder by the sentinel number,
// and returns the placeholders found in order. Other strings containing "{{" are an error.
func sentinelJSON(raw []byte) ([]byte, []int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, nil, fmt.Errorf("body is not valid JSON: %v", err)
	}
	refs := []int{}
	v, err := walkSentinels(v, &refs)
	if err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(v)
	return out, refs, err
}

func walkSentinels(v any, refs *[]int) (any, error) {
	switch t := v.(type) {
	case string:
		if n, ok := wholePlaceholder(t); ok {
			*refs = append(*refs, n)
			return json.Number(sentinel(n)), nil
		}
		if strings.Contains(t, "{{") {
			return nil, errPlaceholderText
		}
		return t, nil
	case map[string]any:
		for k, x := range t {
			y, err := walkSentinels(x, refs)
			if err != nil {
				return nil, err
			}
			t[k] = y
		}
		return t, nil
	case []any:
		for i, x := range t {
			y, err := walkSentinels(x, refs)
			if err != nil {
				return nil, err
			}
			t[i] = y
		}
		return t, nil
	}
	return v, nil
}

// sentinelText is the same for a path segment or a query value, as text.
func sentinelText(s string, refs *[]int) (string, error) {
	if n, ok := wholePlaceholder(s); ok {
		*refs = append(*refs, n)
		return sentinel(n), nil
	}
	if strings.Contains(s, "{{") {
		return "", errPlaceholderText
	}
	return s, nil
}

// sentinelPath replaces whole placeholder segments of a path.
func sentinelPath(p string, refs *[]int) (string, error) {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		v, err := sentinelText(s, refs)
		if err != nil {
			return "", err
		}
		segs[i] = v
	}
	return strings.Join(segs, "/"), nil
}

// sentinelQuery replaces whole placeholder values of a query.
func sentinelQuery(q map[string]string, refs *[]int) (map[string]string, error) {
	if q == nil {
		return nil, nil
	}
	out := make(map[string]string, len(q))
	for k, v := range q {
		if strings.Contains(k, "{{") {
			return nil, errPlaceholderText
		}
		sv, err := sentinelText(v, refs)
		if err != nil {
			return nil, err
		}
		out[k] = sv
	}
	return out, nil
}

// restoreID turns an exact sentinel back into its placeholder.
func restoreID(s string) (string, bool, error) {
	if !sentinelOK.MatchString(s) {
		return s, false, nil
	}
	n, err := strconv.Atoi(s[8:])
	if err != nil || n < 1 {
		return "", false, errPlaceholderText
	}
	return fmt.Sprintf("{{step%d.id}}", n), true, nil
}

// restorePath turns sentinel segments back into placeholders; a sentinel inside a segment is an error.
func restorePath(p string) (string, error) {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		r, ok, err := restoreID(s)
		if err != nil {
			return "", err
		}
		if ok {
			segs[i] = r
		} else if sentinelRe.MatchString(s) {
			return "", errPlaceholderText
		}
	}
	return strings.Join(segs, "/"), nil
}

func restoreQuery(q map[string]string) (map[string]string, error) {
	if q == nil {
		return nil, nil
	}
	out := make(map[string]string, len(q))
	for k, v := range q {
		r, ok, err := restoreID(v)
		if err != nil {
			return nil, err
		}
		if !ok {
			if sentinelRe.MatchString(v) {
				return nil, errPlaceholderText
			}
			r = v
		}
		out[k] = r
	}
	return out, nil
}

// restoreJSON turns sentinel numbers (bare or quoted) in canonical JSON into quoted placeholders.
// A sentinel inside a longer text leaves invalid JSON behind and is refused.
func restoreJSON(s string) (string, error) {
	locs := sentinelRe.FindAllStringIndex(s, -1)
	if locs == nil {
		return s, nil
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		start, end := l[0], l[1]
		n, err := strconv.Atoi(s[start+8 : end])
		if err != nil || n < 1 {
			return "", errPlaceholderText
		}
		from, to := start, end
		if start > 0 && s[start-1] == '"' && end < len(s) && s[end] == '"' {
			from, to = start-1, end+1
		}
		b.WriteString(s[last:from])
		fmt.Fprintf(&b, `"{{step%d.id}}"`, n)
		last = to
	}
	b.WriteString(s[last:])
	out := b.String()
	if !json.Valid([]byte(out)) {
		return "", errPlaceholderText
	}
	return out, nil
}

// substPath puts resolved ids into a stored path.
func substPath(p string, resolve Resolve) (string, bool) {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if n, ok := wholePlaceholder(s); ok {
			id, ok := resolve(n)
			if !ok || !idRe.MatchString(id) {
				return "", false
			}
			segs[i] = id
		} else if strings.Contains(s, "{{") {
			return "", false
		}
	}
	return strings.Join(segs, "/"), true
}

// substJSON puts resolved ids into a stored canonical body (body = true: a numeric id becomes a
// JSON number) or query (always a JSON string).
func substJSON(s string, resolve Resolve, body bool) (string, bool) {
	if s == "" {
		return "", true
	}
	locs := canonRefRe.FindAllStringSubmatchIndex(s, -1)
	var b strings.Builder
	last := 0
	for _, l := range locs {
		n, _ := strconv.Atoi(s[l[2]:l[3]])
		id, ok := resolve(n)
		if !ok || !idRe.MatchString(id) {
			return "", false
		}
		b.WriteString(s[last:l[0]])
		if body && digitsRe.MatchString(id) && (id == "0" || id[0] != '0') {
			b.WriteString(id)
		} else {
			b.WriteString(`"` + id + `"`)
		}
		last = l[1]
	}
	b.WriteString(s[last:])
	if strings.Contains(b.String(), "{{") && anyRefRe.MatchString(b.String()) {
		return "", false
	}
	return b.String(), true
}

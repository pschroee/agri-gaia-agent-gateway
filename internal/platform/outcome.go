// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// CallOutcome is how the gateway counts the result of one call of the agent against the limit for failed tool
// calls in a row (#217).
type CallOutcome int

const (
	// OutcomeNeutral: neither a failure nor a success; the call neither counts nor resets. The user's decision
	// (rejected, still pending) is theirs: counting it would let five rejections stop the agent instead of the user.
	OutcomeNeutral CallOutcome = iota
	// OutcomeOK: the call went through; it resets the counters.
	OutcomeOK
	// OutcomeFailed: the agent has to do something differently: an error, a denial, an unavailable capability, or
	// an answer with an HTTP status of 400 and above.
	OutcomeFailed
)

// OutcomeOf classifies the result of one call. A Go error is a failure; an answer with status ok counts as a
// failure when the platform answered HTTP 400 or above.
func OutcomeOf(res Result, err error) CallOutcome {
	if err != nil {
		return OutcomeFailed
	}
	switch res.Status {
	case "rejected", StatusPendingUser:
		return OutcomeNeutral
	case "error", "denied", StatusUnavailable:
		return OutcomeFailed
	}
	if res.HTTPStatus >= 400 {
		return OutcomeFailed
	}
	return OutcomeOK
}

// AppendAgentNote adds a sentence of the gateway to what the agent reads of the result. Result.Text prints only the
// message of an answer without an HTTP status (and of rejected, denied, unavailable and pending answers); in every
// other case the note comes after the answer, and the REST binding sends it as X-Agw-Note.
func AppendAgentNote(res Result, note string) Result {
	if note == "" {
		return res
	}
	if res.HTTPStatus == 0 || (res.Status != "ok" && res.Status != "error") {
		if res.Message == "" {
			res.Message = note
		} else {
			res.Message += "\n" + note
		}
		return res
	}
	if res.Note == "" {
		res.Note = note
	} else {
		res.Note += "\n" + note
	}
	return res
}

// keyEntry is a REST form of a tool as a pattern.
type keyEntry struct {
	method string
	re     *regexp.Regexp
	tool   string
}

var placeholderRe = regexp.MustCompile(`\{[a-z_]+\}`)

// pathPattern turns "/datasets/{dataset_id}" into an anchored pattern in which a placeholder is one path segment.
func pathPattern(path string, fold bool) *regexp.Regexp {
	var b strings.Builder
	if fold {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	rest := path
	for {
		loc := placeholderRe.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		b.WriteString(regexp.QuoteMeta(rest[:loc[0]]))
		b.WriteString("[^/]+")
		rest = rest[loc[1]:]
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// keyEntries are the REST forms of all tools, the gateway's own paths first (restCalls), then the platform's
// (apiRefs). In the order of the tool names, so that two tools with the same form (get_model and validate_model
// both read GET /models/{model_id}) always resolve to the same key.
var keyEntries = sync.OnceValue(func() []keyEntry {
	var out []keyEntry
	names := func(m map[string]string) []string {
		var l []string
		for k := range m {
			l = append(l, k)
		}
		sort.Strings(l)
		return l
	}
	var gw []string
	for k := range restCalls {
		gw = append(gw, k)
	}
	sort.Strings(gw)
	for _, n := range gw {
		c := restCalls[n]
		out = append(out, keyEntry{method: c.Method, re: pathPattern(c.Path, true), tool: n})
	}
	for _, n := range names(apiRefs) {
		method, path, ok := strings.Cut(apiRefs[n], " ")
		if !ok {
			continue
		}
		out = append(out, keyEntry{method: method, re: pathPattern(path, false), tool: n})
	}
	return out
})

var (
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// CallKey is the counter a REST call belongs to: the name of the tool whose REST form the call has (so that
// GET /datasets/5 and the tool get_dataset are one counter in every binding), otherwise "METHOD /path" with
// numeric and UUID segments as {id}.
func CallKey(method, path string) string {
	method = strings.ToUpper(method)
	p := path
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	for _, e := range keyEntries() {
		if e.method == method && e.re.MatchString(p) {
			return e.tool
		}
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if digitsRe.MatchString(s) || uuidRe.MatchString(s) {
			segs[i] = "{id}"
		}
	}
	return method + " " + strings.Join(segs, "/")
}

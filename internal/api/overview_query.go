// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parameters the overviews across chats share (experiments, plans): chat, period, sort, order and page.
const (
	overviewDefaultLimit = 25
	overviewMaxLimit     = 100
)

var overviewUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// OverviewQuery is the parsed common part of the query of an overview across chats.
type OverviewQuery struct {
	ChatID       string
	Since, Until time.Time
	Sort, Order  string
	Offset       int
	Limit        int
}

// parseOverviewQuery reads chat, since, until, sort, order, offset and limit. sorts lists the sort keys the
// overview knows (nil: the overview does not sort and ignores sort and order). The second value is the text of
// the 400 answer; empty means the query is good.
func parseOverviewQuery(q url.Values, sorts []string) (OverviewQuery, string) {
	o := OverviewQuery{Limit: overviewDefaultLimit}
	if v := q.Get("chat"); v != "" {
		if !overviewUUID.MatchString(strings.ToLower(v)) {
			return o, "chat: a chat id (uuid)"
		}
		o.ChatID = strings.ToLower(v)
	}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"since", &o.Since}, {"until", &o.Until}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return o, p.name + ": expected an RFC 3339 time such as 2026-10-06T00:00:00Z"
			}
			*p.dst = t
		}
	}
	if len(sorts) > 0 {
		if v := q.Get("sort"); v != "" {
			ok := false
			for _, s := range sorts {
				ok = ok || s == v
			}
			if !ok {
				return o, "sort: one of " + strings.Join(sorts, ", ")
			}
			o.Sort = v
		}
		switch v := q.Get("order"); v {
		case "":
			if o.Sort == "started" || o.Sort == "duration" {
				o.Order = "desc"
			} else if o.Sort != "" {
				o.Order = "asc"
			}
		case "asc", "desc":
			o.Order = v
		default:
			return o, "order: asc or desc"
		}
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return o, "offset: 0 or more"
		}
		o.Offset = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > overviewMaxLimit {
			return o, "limit: 1 to 100"
		}
		o.Limit = n
	}
	return o, ""
}

// parseList reads a comma-separated list parameter whose items must come from allowed. The second value is the
// text of the 400 answer; empty means good. A missing or empty parameter gives a nil list.
func parseList(q url.Values, name string, allowed []string) ([]string, string) {
	v := q.Get(name)
	if v == "" {
		return nil, ""
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		ok := false
		for _, a := range allowed {
			ok = ok || a == item
		}
		if !ok {
			return nil, name + ": one of " + strings.Join(allowed, ", ")
		}
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out, ""
}

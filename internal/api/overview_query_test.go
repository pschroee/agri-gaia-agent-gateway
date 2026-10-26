// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/url"
	"testing"
)

func TestParseOverviewQuery(t *testing.T) {
	sorts := []string{"started", "duration", "title", "state", "chat"}
	bad := map[string]string{
		"chat=x":          "chat: a chat id (uuid)",
		"since=yesterday": "since: expected an RFC 3339 time such as 2026-10-06T00:00:00Z",
		"until=1":         "until: expected an RFC 3339 time such as 2026-10-06T00:00:00Z",
		"sort=color":      "sort: one of started, duration, title, state, chat",
		"order=up":        "order: asc or desc",
		"offset=-1":       "offset: 0 or more",
		"limit=0":         "limit: 1 to 100",
		"limit=101":       "limit: 1 to 100",
	}
	for raw, want := range bad {
		q, _ := url.ParseQuery(raw)
		if _, got := parseOverviewQuery(q, sorts); got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
	q, _ := url.ParseQuery("sort=started&limit=50&offset=10&chat=00000000-0000-0000-0000-000000000001")
	o, msg := parseOverviewQuery(q, sorts)
	if msg != "" || o.Sort != "started" || o.Order != "desc" || o.Limit != 50 || o.Offset != 10 || o.ChatID == "" {
		t.Errorf("good query: %+v %q", o, msg)
	}
	if o, _ := parseOverviewQuery(url.Values{}, sorts); o.Limit != 25 {
		t.Errorf("default limit: %d", o.Limit)
	}
	// Without sort keys sort and order are ignored.
	q, _ = url.ParseQuery("sort=color&order=up")
	if _, msg := parseOverviewQuery(q, nil); msg != "" {
		t.Errorf("sort ignored: %q", msg)
	}
}

func TestParseList(t *testing.T) {
	allowed := []string{"a", "b", "c"}
	q, _ := url.ParseQuery("x=a,c,a")
	if got, msg := parseList(q, "x", allowed); msg != "" || len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("good: %v %q", got, msg)
	}
	q, _ = url.ParseQuery("x=a,z")
	if _, msg := parseList(q, "x", allowed); msg != "x: one of a, b, c" {
		t.Errorf("bad: %q", msg)
	}
	if got, msg := parseList(url.Values{}, "x", allowed); got != nil || msg != "" {
		t.Errorf("missing: %v %q", got, msg)
	}
}

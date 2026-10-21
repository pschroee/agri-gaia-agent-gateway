// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestParseInternetArgs(t *testing.T) {
	cases := []struct {
		args []string
		want internetCmd
	}{
		{[]string{"search", "find two sources"}, internetCmd{Action: internetRequest, Kind: "search", Reason: "find two sources"}},
		{[]string{"search", "find", "two", "sources"}, internetCmd{Action: internetRequest, Kind: "search", Reason: "find two sources"}},
		{[]string{"SEARCH", "x"}, internetCmd{Action: internetRequest, Kind: "search", Reason: "x"}},
		{[]string{"domains", "a.example.org,b.example.org", "download the archive", "--minutes", "45", "--mb=800"},
			internetCmd{Action: internetRequest, Kind: "domains", Domains: []string{"a.example.org", "b.example.org"}, Reason: "download the archive", Minutes: 45, MB: 800}},
		{[]string{"domains", "--mb", "5", "a.example.org", "download"}, internetCmd{Action: internetRequest, Kind: "domains", Domains: []string{"a.example.org"}, Reason: "download", MB: 5}},
		// the gateway splits the list itself (extras may hold commas)
		{[]string{"packages", "pip:torch==2.5.1,npm:left-pad", "train a model"}, internetCmd{Action: internetRequest, Kind: "packages", Packages: []string{"pip:torch==2.5.1,npm:left-pad"}, Reason: "train a model"}},
		{[]string{"full", "--", "-v output needed"}, internetCmd{Action: internetRequest, Kind: "full", Reason: "-v output needed"}},
		{[]string{"full", "clone over ssh", "--minutes", "90"}, internetCmd{Action: internetRequest, Kind: "full", Reason: "clone over ssh", Minutes: 90}},
		{[]string{"off"}, internetCmd{Action: internetOff}},
		{[]string{"OFF"}, internetCmd{Action: internetOff}},
		{[]string{"off", "7"}, internetCmd{Action: internetOff, GrantID: 7}},
		{[]string{"status"}, internetCmd{Action: internetStatus}},
		{[]string{"--help"}, internetCmd{Action: internetHelpCmd}},
		{[]string{"-h"}, internetCmd{Action: internetHelpCmd}},
	}
	for _, c := range cases {
		got, err := parseInternetArgs(c.args)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %+v %v, want %+v", c.args, got, err, c.want)
		}
	}
	for _, bad := range [][]string{
		nil, {""}, {"  "}, {"--"},
		{"pip install scikit-learn"},       // the old form: the reason alone
		{"off-site download of a dataset"}, // also the old form, not "off"
		{"search"}, {"search", " "},        // no reason
		{"domains", "a.example.org"}, // list but no reason
		{"domains"}, {"packages"},    // no list
		{"domains", "a.example.org", "x", "--minutes"}, {"full", "x", "--mb", "zero"}, {"full", "x", "--mb", "0"},
		{"off", "now"}, {"off", "0"}, {"off", "1", "2"}, {"status", "x"},
	} {
		if got, err := parseInternetArgs(bad); err == nil {
			t.Errorf("%q: expected an error, got %+v", bad, got)
		}
	}
	// The old form gets the hint on the four kinds.
	if _, err := parseInternetArgs([]string{"pip install x"}); err == nil || !strings.Contains(err.Error(), "search, domains, packages, full") {
		t.Errorf("old form hint: %v", err)
	}
}

// agw-internet off sends POST /internet/off without a body (or with the grant id) and succeeds also for "already
// off"; a request sends kind, reason and list; the old form exits with 2 without reaching the socket; refused: exit 2.
func TestInternetCommandOverSocket(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internet/off":
			_, _ = io.WriteString(w, `{"status":"already_off","name":"internet","message":"internet access was already off"}`)
		case "/internet":
			switch {
			case r.Method == http.MethodGet:
				_, _ = io.WriteString(w, `{"grants":[{"id":7,"kind":"domains","domains":["a.example.org"],"max_bytes":524288000,"bytes":1048576,"blocked":2,"expires_at":"2026-10-21T15:42:00Z"}]}`)
			case strings.Contains(string(body), `"kind":"full"`):
				_, _ = io.WriteString(w, `{"status":"rejected","name":"internet","message":"rejected by the user"}`)
			case strings.Contains(string(body), `"kind":"packages"`):
				_, _ = io.WriteString(w, `{"status":"refused","name":"internet","message":"package access is not available yet"}`)
			default:
				_, _ = io.WriteString(w, `{"status":"approved","name":"internet","message":"search until 15:42 or 500 MB (grant 7)"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	dir, err := os.MkdirTemp("", "agwcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := dir + "/agw.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}

	if code, err := internet(hc, []string{"off"}); code != 0 || err != nil {
		t.Fatalf("off: %d %v", code, err)
	}
	if code, err := internet(hc, []string{"off", "7"}); code != 0 || err != nil {
		t.Fatalf("off 7: %d %v", code, err)
	}
	if code, err := internet(hc, []string{"search", "look something up"}); code != 0 || err != nil {
		t.Fatalf("search: %d %v", code, err)
	}
	if code, err := internet(hc, []string{"full", "--", "off"}); code != exitRejected || err != nil {
		t.Fatalf("full with reason off: %d %v", code, err)
	}
	if code, err := internet(hc, []string{"packages", "pip:torch", "train"}); code != exitUsage || err != nil {
		t.Fatalf("refused: %d %v", code, err)
	}
	if code, err := internet(hc, []string{"status"}); code != 0 || err != nil {
		t.Fatalf("status: %d %v", code, err)
	}
	// Wrong usage and the old form never reach the socket.
	if code, _ := internet(hc, []string{"off", "now"}); code != exitUsage {
		t.Fatalf("off with arguments: %d", code)
	}
	if code, _ := internet(hc, []string{"pip install pandas"}); code != exitUsage {
		t.Fatalf("old form: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"POST /internet/off ",
		`POST /internet/off {"grant_id":7}`,
		`POST /internet {"domains":null,"kind":"search","megabytes":0,"minutes":0,"packages":null,"reason":"look something up"}`,
		`POST /internet {"domains":null,"kind":"full","megabytes":0,"minutes":0,"packages":null,"reason":"off"}`,
		`POST /internet {"domains":null,"kind":"packages","megabytes":0,"minutes":0,"packages":["pip:torch"],"reason":"train"}`,
		"GET /internet ",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests: %q", got)
	}
}

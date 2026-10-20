// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Lines as the training template of the backend fork writes them (one per epoch, the last one repeated with saved).
const templateLines = `{"epoch":0,"epochs":3,"at":"2026-10-20T10:00:10Z","train_loss":1.2,"train_acc":0.4,"val_loss":1.1,"val_acc":0.5,"val_f1":0.45,"lr":0.001}
{"epoch":1,"epochs":3,"at":"2026-10-20T10:00:20Z","train_loss":0.9,"train_acc":0.6,"val_loss":0.8,"val_acc":0.7,"lr":0.001}
{"epoch":2,"epochs":3,"at":"2026-10-20T10:00:30Z","train_loss":0.7,"train_acc":0.7,"val_loss":0.6,"val_acc":0.8,"lr":0.001}
{"epoch":2,"epochs":3,"at":"2026-10-20T10:00:31Z","train_loss":0.7,"train_acc":0.7,"val_loss":0.6,"val_acc":0.8,"lr":0.001,"saved":true}
`

func TestParseMetricsJSONLTemplateLines(t *testing.T) {
	got := ParseMetricsJSONL([]byte(templateLines))
	if len(got) != 3 {
		t.Fatalf("the repeated last line must replace its epoch, got %d lines: %+v", len(got), got)
	}
	last := got[2]
	if last.Epoch != 2 || last.Epochs != 3 || !last.Saved || last.Values["val_acc"] != 0.8 {
		t.Errorf("last line: %+v", last)
	}
	if !last.At.Equal(time.Date(2026, 10, 20, 10, 0, 31, 0, time.UTC)) {
		t.Errorf("at: %v", last.At)
	}
	if _, ok := got[1].Values["val_f1"]; ok || got[0].Values["val_f1"] != 0.45 {
		t.Errorf("a missing figure stays missing: %+v %+v", got[0], got[1])
	}
	if _, ok := last.Values["epoch"]; ok {
		t.Error("epoch is no figure")
	}
}

func TestParseMetricsJSONLDropsBadLines(t *testing.T) {
	long := `{"epoch":1,"x":"` + strings.Repeat("a", MaxMetricsLine) + `"}`
	bad := []string{
		`{"epoch":1,"epoch":2}`,          // duplicate key
		`{"epoch":1,"val_acc":NaN}`,      // not JSON
		`{"epoch":1,"val_acc":Infinity}`, // not JSON
		`{"epoch":1,"val_acc":1e999}`,    // still a line: the figure falls away, see below
		`{"epoch":-1}`,                   // negative
		`{"epoch":1.5}`,                  // not whole
		`{"val_acc":0.5}`,                // no epoch
		`{"epoch":1,"epochs":0}`,         // epochs < 1
		`{"epoch":1,"at":"yesterday"}`,   // no time
		`{"epoch":1,"saved":"yes"}`,      // no bool
		`{"epoch":1,"Val_Acc":0.5}`,      // key shape
		`{"epoch":1,"1x":0.5}`,           // key shape
		`[1,2]`, `"x"`, `not json`, long, // no object / too long
		`{"epoch":1} {"epoch":2}`, // data after the value
	}
	got := ParseMetricsJSONL([]byte(strings.Join(bad, "\n")))
	// Only the 1e999 line survives, without its figure.
	if len(got) != 1 || got[0].Epoch != 1 || len(got[0].Values) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMetricsJSONLKeepsFiguresOnly(t *testing.T) {
	got := ParseMetricsJSONL([]byte(`{"epoch":0,"val_acc":0.5,"note":"hi","flag":true,"nested":{"a":1},"nothing":null}`))
	if len(got) != 1 || len(got[0].Values) != 1 || got[0].Values["val_acc"] != 0.5 {
		t.Fatalf("got %+v", got)
	}
	// 3.0 is a whole number.
	if got := ParseMetricsJSONL([]byte(`{"epoch":3.0,"epochs":10.0}`)); len(got) != 1 || got[0].Epoch != 3 || got[0].Epochs != 10 {
		t.Errorf("whole floats: %+v", got)
	}
}

func TestParseMetricsJSONLTooManyKeysAndLines(t *testing.T) {
	var keys []string
	for i := 0; i <= MaxMetricsKeys; i++ {
		keys = append(keys, `"k`+strings.Repeat("a", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))+`":1`)
	}
	if got := ParseMetricsJSONL([]byte(`{"epoch":0,` + strings.Join(keys, ",") + `}`)); len(got) != 0 {
		t.Errorf("a line with more than %d keys: %+v", MaxMetricsKeys, got)
	}
	var b strings.Builder
	for i := 0; i < MaxMetricsLines+50; i++ {
		b.WriteString(`{"epoch":` + itoa(i) + "}\n")
	}
	got := ParseMetricsJSONL([]byte(b.String()))
	if len(got) != MaxMetricsLines || got[len(got)-1].Epoch != MaxMetricsLines+49 {
		t.Errorf("the last %d lines are read: %d lines, last %+v", MaxMetricsLines, len(got), got[len(got)-1])
	}
}

func TestParseMetricsJSONLOrdersAndDeduplicatesEpochs(t *testing.T) {
	// A tuner run writes extra, out-of-order lines.
	got := ParseMetricsJSONL([]byte("{\"epoch\":1,\"val_acc\":0.1}\n{\"epoch\":0,\"val_acc\":0.2}\n{\"epoch\":1,\"val_acc\":0.3}\n"))
	if len(got) != 2 || got[0].Epoch != 0 || got[1].Epoch != 1 || got[1].Values["val_acc"] != 0.3 {
		t.Errorf("got %+v", got)
	}
}

func TestParseGPUCSV(t *testing.T) {
	raw := "2026/10/20 10:00:05.123, 45, 1234, 8192\r\n" +
		"2026-10-20T10:00:10Z, 100, 2000, 8192\n" +
		"2026/10/20 10:00:15.000, [N/A], 1, 2\n" + // not a number
		"2026/10/20 10:00:20.000, 101, 1, 2\n" + // above 100
		"2026/10/20 10:00:25.000, 50, 1\n" + // three fields
		"2026/10/20 10:00:30.000, 50, -1, 2\n" + // negative memory
		"later, 50, 1, 2\n" + // no time
		"\n"
	got := ParseGPUCSV([]byte(raw))
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Util != 45 || got[0].MemUsedMB != 1234 || got[0].MemTotalMB != 8192 ||
		!got[0].At.Equal(time.Date(2026, 10, 20, 10, 0, 5, 123e6, time.UTC)) {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Util != 100 {
		t.Errorf("second: %+v", got[1])
	}
	var b strings.Builder
	for i := 0; i < MaxGPULines+10; i++ {
		b.WriteString("2026-10-20T10:00:00Z, 1, 1, 1\n")
	}
	if n := len(ParseGPUCSV([]byte(b.String()))); n != MaxGPULines {
		t.Errorf("lines: %d", n)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

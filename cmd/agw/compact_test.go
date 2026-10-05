package main

import (
	"strings"
	"testing"

	"agw/internal/agwclient"
)

func TestChatCompactWait(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"compaction_start","reason":"manual"}}`,
		`{"kind":"pi","data":{"type":"compaction_end","reason":"manual","aborted":false,"result":{"summary":"…","tokensBefore":42000,"estimatedTokensAfter":3100,"usage":{"input":42000,"output":800}}}}`,
	}
	code, out, errw := f.run("chat", "compact", "c1", "focus", "on", "code", "--wait")
	if code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, errw)
	}
	if !strings.Contains(out, "context summarised: 42,000 → approx. 3,100 tokens") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errw, "summarising the context") {
		t.Errorf("compaction_start not visible: %q", errw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) != 1 || f.commands[0] != "/compact focus on code" {
		t.Errorf("commands = %q", f.commands)
	}
}

func TestChatCompactWaitAborted(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"compaction_start","reason":"manual"}}`,
		`{"kind":"pi","data":{"type":"compaction_end","reason":"manual","aborted":true,"result":null,"errorMessage":"too short"}}`,
	}
	code, _, errw := f.run("chat", "cmd", "c1", "/compact", "--wait")
	if code != 1 || !strings.Contains(errw, "too short") {
		t.Errorf("code=%d stderr=%q", code, errw)
	}
}

func TestChatCmdOtherWaitsForSettled(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"agent_start"}}`,
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"report done"}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
	}
	code, out, errw := f.run("chat", "cmd", "c1", "/skill:report short", "--wait")
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errw)
	}
	if !strings.HasPrefix(out, "report done") {
		t.Errorf("stdout = %q", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) != 1 || f.commands[0] != "/skill:report short" {
		t.Errorf("commands = %q", f.commands)
	}
}

func TestChatCmdWithoutSlashRejected(t *testing.T) {
	f := newFakeServer(t)
	if code, _, _ := f.run("chat", "cmd", "c1", "compact"); code != 2 {
		t.Errorf("without / expected exit 2, got %d", code)
	}
}

func TestChatCommandsList(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "commands", "c1")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{"Name", "Source", "Description", "/compact [instructions]", "built-in", "summarise the context now", "/skill:report", "skill"} {
		if !strings.Contains(out, want) {
			t.Errorf("table without %q:\n%s", want, out)
		}
	}
}

func TestChatAutocompact(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "autocompact", "c1", "off")
	if code != 0 {
		t.Fatal(errw)
	}
	if !strings.Contains(out, "Auto-compaction for chat c1 off") {
		t.Errorf("stdout = %q", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.autocompact) != 1 || f.autocompact[0] {
		t.Errorf("autocompact = %v", f.autocompact)
	}
}

var internetScript = []string{
	`{"kind":"pi","data":{"type":"agent_start"}}`,
	`{"kind":"approval","data":{"id":"n1","chat_id":"c1","kind":"internet_access","via":"mcp","name":"pip install pandas","size":0,"state":"pending"}}`,
	"WAIT_DECISION",
	`{"kind":"approval","data":{"id":"n1","chat_id":"c1","kind":"internet_access","via":"mcp","name":"pip install pandas","size":0,"state":"approved"}}`,
	`{"kind":"pi","data":{"type":"agent_settled"}}`,
}

func TestRunInternetAccessAutoApprove(t *testing.T) {
	f := newFakeServer(t)
	f.script = internetScript
	code, _, errw := f.run("run", "--auto-approve", "Install pandas")
	if code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, errw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 1 || f.decisions[0] != (decision{"n1", true}) {
		t.Errorf("decisions = %+v", f.decisions)
	}
	for _, want := range []string{"agent requests internet access: pip install pandas", "internet access approved automatically"} {
		if !strings.Contains(errw, want) {
			t.Errorf("stderr without %q:\n%s", want, errw)
		}
	}
	if strings.Contains(errw, "bytes") {
		t.Errorf("internet access described with a file size: %s", errw)
	}
}

func TestStreamInternetAccessAsk(t *testing.T) {
	s, _, errw, fd := newTestStreamer(approvalAsk, "y\n")
	feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(`{"id":"n2","chat_id":"c1","kind":"internet_access","via":"cli","name":"load package","size":0,"state":"pending","preview":"load package"}`)})
	e := errw.String()
	if !strings.Contains(e, "agent requests internet access: load package") || !strings.Contains(e, "Allow internet access? [y/n]") {
		t.Errorf("stderr = %q", e)
	}
	if strings.Contains(e, "preview") {
		t.Errorf("preview for internet access: %q", e)
	}
	if len(fd.calls) != 1 || !fd.calls[0].approve {
		t.Errorf("decisions = %+v", fd.calls)
	}
}

func TestStreamAutoCompactionDuringRun(t *testing.T) {
	s, out, errw, _ := newTestStreamer(approvalShow, "")
	done := feed(t, s, true,
		piEv(`{"type":"agent_start"}`),
		piEv(`{"type":"compaction_start","reason":"threshold"}`),
		piEv(`{"type":"compaction_end","reason":"threshold","aborted":false,"result":{"tokensBefore":990000,"estimatedTokensAfter":12000}}`),
	)
	if done {
		t.Error("compaction_end must not end the waiting without a compaction request")
	}
	e := errw.String()
	if !strings.Contains(e, "threshold") || !strings.Contains(e, "context summarised: 990,000 → approx. 12,000 tokens") {
		t.Errorf("stderr = %q", e)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q", out.String())
	}
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func TestFmtContext(t *testing.T) {
	c := agwclient.Chat{AutoCompact: true, Compactions: 1, Context: &agwclient.ContextUsage{
		Tokens: i64(4200), Window: 1000000, Percent: f64(0.42), ThresholdTokens: 983616}}
	want := "context 4,200 / 1,000,000 tokens (0.4 %), auto-compaction on from 983,616, 1 compaction"
	if got := fmtContext(c); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	c = agwclient.Chat{AutoCompact: false, Compactions: 2, Context: &agwclient.ContextUsage{Window: 128000, ThresholdTokens: 111616}}
	want = "context ? / 128,000 tokens (measured again after the next reply), auto-compaction off, 2 compactions"
	if got := fmtContext(c); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	c = agwclient.Chat{AutoCompact: true}
	want = "context not measured yet, auto-compaction on"
	if got := fmtContext(c); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

func TestFmtInt(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 983616: "983,616", 1000000: "1,000,000", -4200: "-4,200"} {
		if got := fmtInt(in); got != want {
			t.Errorf("fmtInt(%d) = %q, want %q", in, got, want)
		}
	}
}

const compactionDetail = `{"chat":{"id":"c1","title":"long","model":"deepseek/deepseek-flash","variant":"cli","state":"active","auto_compact":true,"compactions":1,
"context":{"tokens":4200,"window":1000000,"percent":0.42,"threshold_tokens":983616,"reserve_tokens":16384,"keep_recent_tokens":20000,"updated_at":"2026-09-29T10:00:00Z"},
"running":false,"tokens":{"input":50000,"output":1000,"cache_read":0,"total":51000},"cost":0.0321,"artifact_count":0,"pending_approvals":0},
"messages":[{"seq":1,"role":"user","message":{"role":"user","content":[{"type":"text","text":"Analyse"}]}},
{"seq":2,"role":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"cost":0.0012,"peak":false},
{"seq":3,"role":"compaction","message":{"role":"compaction","reason":"manual","summary":"Summary","tokensBefore":42000,"estimatedTokensAfter":3100,"timestamp":1},"cost":0.0009,"peak":true},
{"seq":4,"role":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Continuing."}]},"cost":0.0030,"peak":true}],
"artifacts":[],"approvals":[{"id":"n1","chat_id":"c1","kind":"internet_access","via":"mcp","name":"pip install pandas","size":0,"state":"pending"}],"socket_calls":[]}`

func TestChatShowCompaction(t *testing.T) {
	f := newFakeServer(t)
	f.detail = compactionDetail
	code, out, errw := f.run("chat", "show", "c1")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{
		"context 4,200 / 1,000,000 tokens (0.4 %), auto-compaction on from 983,616, 1 compaction",
		"cost 0.0321 USD",
		"context summarised (manual): 42,000 → approx. 3,100 tokens",
		"0.0012 USD (off-peak tariff)",
		"0.0030 USD (peak tariff)",
		"Internet access: pip install pandas",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Summary\n") {
		t.Errorf("summary printed unasked:\n%s", out)
	}
	// Order: the compaction is between the two replies.
	if i, j, k := strings.Index(out, "Done."), strings.Index(out, "context summarised"), strings.Index(out, "Continuing."); !(i < j && j < k) {
		t.Errorf("wrong order:\n%s", out)
	}
}

func TestModelsTariff(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("models")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"now", "off-peak", "0.135", "0.55", "Mon–Fri 01:00–09:00 UTC"} {
		if !strings.Contains(out, want) {
			t.Errorf("table without %q:\n%s", want, out)
		}
	}
}

func TestActivityCompacting(t *testing.T) {
	if got := activityLabel(&agwclient.Activity{Kind: "compacting"}); got != "Summarising the context" {
		t.Errorf("activityLabel = %q", got)
	}
}

func TestFmtWorkspace(t *testing.T) {
	if got := fmtWorkspace(nil); got != "workspace not saved yet" {
		t.Errorf("nil: %q", got)
	}
	w := &agwclient.Workspace{Size: 1258291, Files: 14, SavedAt: "2026-09-29T17:05:00Z"}
	if got := fmtWorkspace(w); !strings.HasPrefix(got, "workspace saved: 1.2 MB, 14 files (2026-09-29 ") {
		t.Errorf("saved: %q", got)
	}
	w.SkippedReason, w.SkippedAt = "312.0 MB in /workspace, limit 200.0 MB", "2026-09-29T18:00:00Z"
	if got := fmtWorkspace(w); !strings.Contains(got, "last time NOT saved") || !strings.Contains(got, "limit 200.0 MB") {
		t.Errorf("skipped: %q", got)
	}
}

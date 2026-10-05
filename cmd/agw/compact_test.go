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
	code, out, errw := f.run("chat", "compact", "c1", "Fokus", "auf", "Code", "--wait")
	if code != 0 {
		t.Fatalf("Exit-Code %d, stderr: %s", code, errw)
	}
	if !strings.Contains(out, "Kontext zusammengefasst: 42.000 → ca. 3.100 Tokens") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errw, "Kontext wird zusammengefasst") {
		t.Errorf("compaction_start nicht sichtbar: %q", errw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) != 1 || f.commands[0] != "/compact Fokus auf Code" {
		t.Errorf("Befehle = %q", f.commands)
	}
}

func TestChatCompactWaitAborted(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"compaction_start","reason":"manual"}}`,
		`{"kind":"pi","data":{"type":"compaction_end","reason":"manual","aborted":true,"result":null,"errorMessage":"zu kurz"}}`,
	}
	code, _, errw := f.run("chat", "cmd", "c1", "/compact", "--wait")
	if code != 1 || !strings.Contains(errw, "zu kurz") {
		t.Errorf("code=%d stderr=%q", code, errw)
	}
}

func TestChatCmdOtherWaitsForSettled(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"agent_start"}}`,
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Bericht fertig"}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
	}
	code, out, errw := f.run("chat", "cmd", "c1", "/skill:bericht kurz", "--wait")
	if code != 0 {
		t.Fatalf("Exit-Code %d: %s", code, errw)
	}
	if !strings.HasPrefix(out, "Bericht fertig") {
		t.Errorf("stdout = %q", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) != 1 || f.commands[0] != "/skill:bericht kurz" {
		t.Errorf("Befehle = %q", f.commands)
	}
}

func TestChatCmdWithoutSlashRejected(t *testing.T) {
	f := newFakeServer(t)
	if code, _, _ := f.run("chat", "cmd", "c1", "compact"); code != 2 {
		t.Errorf("ohne / erwartet Exit 2, erhalten %d", code)
	}
}

func TestChatCommandsList(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "commands", "c1")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{"Name", "Quelle", "Beschreibung", "/compact [Anweisungen]", "eingebaut", "Kontext jetzt zusammenfassen", "/skill:bericht", "Skill"} {
		if !strings.Contains(out, want) {
			t.Errorf("Tabelle ohne %q:\n%s", want, out)
		}
	}
}

func TestChatAutocompact(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "autocompact", "c1", "off")
	if code != 0 {
		t.Fatal(errw)
	}
	if !strings.Contains(out, "Auto-Kompaktierung für Chat c1 aus") {
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
	code, _, errw := f.run("run", "--auto-approve", "Installiere pandas")
	if code != 0 {
		t.Fatalf("Exit-Code %d, stderr: %s", code, errw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 1 || f.decisions[0] != (decision{"n1", true}) {
		t.Errorf("Entscheidungen = %+v", f.decisions)
	}
	for _, want := range []string{"Agent bittet um Internetzugang: pip install pandas", "Internetzugang automatisch bestätigt"} {
		if !strings.Contains(errw, want) {
			t.Errorf("stderr ohne %q:\n%s", want, errw)
		}
	}
	if strings.Contains(errw, "Bytes") {
		t.Errorf("Internetzugang mit Dateigröße beschrieben: %s", errw)
	}
}

func TestStreamInternetAccessAsk(t *testing.T) {
	s, _, errw, fd := newTestStreamer(approvalAsk, "j\n")
	feed(t, s, true, agwclient.Event{Kind: "approval", Data: []byte(`{"id":"n2","chat_id":"c1","kind":"internet_access","via":"cli","name":"Paket laden","size":0,"state":"pending","preview":"Paket laden"}`)})
	e := errw.String()
	if !strings.Contains(e, "Agent bittet um Internetzugang: Paket laden") || !strings.Contains(e, "Internetzugang erlauben? [j/n]") {
		t.Errorf("stderr = %q", e)
	}
	if strings.Contains(e, "Vorschau") {
		t.Errorf("Vorschau bei Internetzugang: %q", e)
	}
	if len(fd.calls) != 1 || !fd.calls[0].approve {
		t.Errorf("Entscheidungen = %+v", fd.calls)
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
		t.Error("compaction_end darf ohne Kompaktierungswunsch das Warten nicht beenden")
	}
	e := errw.String()
	if !strings.Contains(e, "Schwelle") || !strings.Contains(e, "Kontext zusammengefasst: 990.000 → ca. 12.000 Tokens") {
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
	want := "Kontext 4.200 / 1.000.000 Tokens (0,4 %), Auto-Kompaktierung an ab 983.616, 1 Kompaktierung"
	if got := fmtContext(c); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	c = agwclient.Chat{AutoCompact: false, Compactions: 2, Context: &agwclient.ContextUsage{Window: 128000, ThresholdTokens: 111616}}
	want = "Kontext ? / 128.000 Tokens (neu gemessen nach der nächsten Antwort), Auto-Kompaktierung aus, 2 Kompaktierungen"
	if got := fmtContext(c); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	c = agwclient.Chat{AutoCompact: true}
	want = "Kontext noch nicht gemessen, Auto-Kompaktierung an"
	if got := fmtContext(c); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

func TestFmtInt(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1.000", 983616: "983.616", 1000000: "1.000.000", -4200: "-4.200"} {
		if got := fmtInt(in); got != want {
			t.Errorf("fmtInt(%d) = %q, erwartet %q", in, got, want)
		}
	}
}

const compactionDetail = `{"chat":{"id":"c1","title":"lang","model":"deepseek/deepseek-flash","variant":"cli","state":"active","auto_compact":true,"compactions":1,
"context":{"tokens":4200,"window":1000000,"percent":0.42,"threshold_tokens":983616,"reserve_tokens":16384,"keep_recent_tokens":20000,"updated_at":"2026-09-29T10:00:00Z"},
"running":false,"tokens":{"input":50000,"output":1000,"cache_read":0,"total":51000},"cost":0.0321,"artifact_count":0,"pending_approvals":0},
"messages":[{"seq":1,"role":"user","message":{"role":"user","content":[{"type":"text","text":"Analysiere"}]}},
{"seq":2,"role":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Erledigt."}]},"cost":0.0012,"peak":false},
{"seq":3,"role":"compaction","message":{"role":"compaction","reason":"manual","summary":"Zusammenfassung","tokensBefore":42000,"estimatedTokensAfter":3100,"timestamp":1},"cost":0.0009,"peak":true},
{"seq":4,"role":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Weiter."}]},"cost":0.0030,"peak":true}],
"artifacts":[],"approvals":[{"id":"n1","chat_id":"c1","kind":"internet_access","via":"mcp","name":"pip install pandas","size":0,"state":"pending"}],"socket_calls":[]}`

func TestChatShowCompaction(t *testing.T) {
	f := newFakeServer(t)
	f.detail = compactionDetail
	code, out, errw := f.run("chat", "show", "c1")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{
		"Kontext 4.200 / 1.000.000 Tokens (0,4 %), Auto-Kompaktierung an ab 983.616, 1 Kompaktierung",
		"Kosten 0,0321 USD",
		"Kontext zusammengefasst (manuell): 42.000 → ca. 3.100 Tokens",
		"0,0012 USD (Nebentarif)",
		"0,0030 USD (Spitzentarif)",
		"Internetzugang: pip install pandas",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Ausgabe ohne %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Zusammenfassung\n") {
		t.Errorf("Zusammenfassung ungefragt ausgegeben:\n%s", out)
	}
	// Reihenfolge: Kompaktierung steht zwischen den beiden Antworten.
	if i, j, k := strings.Index(out, "Erledigt."), strings.Index(out, "Kontext zusammengefasst"), strings.Index(out, "Weiter."); !(i < j && j < k) {
		t.Errorf("Reihenfolge falsch:\n%s", out)
	}
}

func TestModelsTariff(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("models")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"jetzt", "Neben", "0,135", "0,55", "Mo–Fr 01:00–09:00 UTC"} {
		if !strings.Contains(out, want) {
			t.Errorf("Tabelle ohne %q:\n%s", want, out)
		}
	}
}

func TestActivityCompacting(t *testing.T) {
	if got := activityLabel(&agwclient.Activity{Kind: "compacting"}); got != "Fasst den Kontext zusammen" {
		t.Errorf("activityLabel = %q", got)
	}
}

func TestFmtWorkspace(t *testing.T) {
	if got := fmtWorkspace(nil); got != "Arbeitsbereich noch nicht gesichert" {
		t.Errorf("nil: %q", got)
	}
	w := &agwclient.Workspace{Size: 1258291, Files: 14, SavedAt: "2026-09-29T17:05:00Z"}
	if got := fmtWorkspace(w); !strings.HasPrefix(got, "Arbeitsbereich gesichert: 1,2 MB, 14 Dateien (2026-09-29 ") {
		t.Errorf("gesichert: %q", got)
	}
	w.SkippedReason, w.SkippedAt = "312,0 MB in /workspace, Grenze 200,0 MB", "2026-09-29T18:00:00Z"
	if got := fmtWorkspace(w); !strings.Contains(got, "zuletzt NICHT gesichert") || !strings.Contains(got, "Grenze 200,0 MB") {
		t.Errorf("ausgelassen: %q", got)
	}
}

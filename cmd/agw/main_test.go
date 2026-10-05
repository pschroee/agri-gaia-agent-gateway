package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer simuliert den Orchestrator: REST-Endpunkte und einen scriptbaren SSE-Strom,
// der erst nach dem Senden einer Nachricht loslegt.
type fakeServer struct {
	t          *testing.T
	srv        *httptest.Server
	subscribed atomic.Bool
	posted     chan string
	decided    chan decision
	noSlot     bool
	running    bool
	script     []string // SSE-Ereignisse nach dem Senden; "WAIT_DECISION" wartet auf eine Entscheidung

	detail    string // Antwort von GET /api/chats/{id}, leer = Standard
	sendReply string // Antwort von POST …/messages, leer = gesendet

	mu          sync.Mutex
	createReq   map[string]any
	decisions   []decision
	commands    []string
	autocompact []bool
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, posted: make(chan string, 4), decided: make(chan decision, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"deepseek/deepseek-flash","provider":"deepseek","model":"deepseek-flash","name":"DeepSeek Flash","default":true,"pricing":{"input":0.27,"output":1.1,"cache_read":0.07,"cache_write":0,"currency":"USD","note":"Spitzentarif","retrieved":"2026-09-28"},"tariff":{"peak_windows_utc":[{"days":"mon-fri","from":"01:00","to":"09:00"}],"offpeak_factor":0.5},"peak_now":false},{"id":"x/y","provider":"x","model":"y","name":"Ohne Preis","default":false}]`)
	})
	mux.HandleFunc("GET /api/pool", func(w http.ResponseWriter, r *http.Request) {
		since := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"slots":[{"id":"p-1","variant":"cli","state":"idle","container_id":"abcdef123456","container_name":"agw-p-1","image":"agw-basis","created_at":%q},{"id":"p-2","variant":"cli","state":"assigned","container_id":"123456abcdef","container_name":"agw-p-2","image":"agw-basis","created_at":%q,"assigned_at":%q,"chat_id":"c1","chat_title":"Test","activity":{"kind":"tool","tool":"bash","since":%q},"internet":false}],"targets":{"cli":2,"mcp":1},"totals":{"cost":0.0123,"tokens":{"input":100,"output":50,"cache_read":10,"total":160},"chats_active":1}}`, since, since, since, since)
	})
	mux.HandleFunc("POST /api/chats", func(w http.ResponseWriter, r *http.Request) {
		if f.noSlot {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"no idle slot"}`)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.createReq = req
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"c1","title":"neu","model":"deepseek/deepseek-flash","variant":"cli","state":"active","tokens":{"input":0,"output":0,"cache_read":0,"total":0}}`)
	})
	mux.HandleFunc("GET /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.detail != "" {
			io.WriteString(w, f.detail)
			return
		}
		fmt.Fprintf(w, `{"chat":{"id":"c1","title":"neu","model":"deepseek/deepseek-flash","variant":"cli","state":"active","running":%v,"tokens":{"input":1200,"output":300,"cache_read":0,"total":1500},"cost":0.0042,"artifact_count":1,"pending_approvals":0},
"messages":[{"seq":1,"role":"user","message":{"role":"user","content":[{"type":"text","text":"Zähle Dateien"}]}},
{"seq":2,"role":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"Ich schaue nach."},{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"ls | wc -l"}}]}},
{"seq":3,"role":"toolResult","message":{"role":"toolResult","toolCallId":"t1","toolName":"bash","content":[{"type":"text","text":"3\n"}],"isError":false}}],
"artifacts":[{"chat_id":"c1","kind":"output","name":"bericht.md","size":120,"via":"cli","created_at":"2026-09-29T10:00:00Z"}],
"approvals":[{"id":"a9","chat_id":"c1","kind":"artifact_upload","via":"mcp","name":"daten.csv","size":42,"state":"pending"}],"socket_calls":[]}`, f.running)
	})
	mux.HandleFunc("GET /api/chats/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Ein alter, abgeschlossener Lauf vor dem Senden darf das Warten nicht beenden.
		io.WriteString(w, ": ping\n\ndata: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_settled\"}}\n\n")
		fl.Flush()
		f.subscribed.Store(true)
		select {
		case <-f.posted:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			return
		}
		for _, line := range f.script {
			if line == "WAIT_DECISION" {
				select {
				case <-f.decided:
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Second):
					return
				}
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /api/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		if !f.subscribed.Load() {
			t.Errorf("Nachricht gesendet, bevor der Ereignisstrom abonniert war")
		}
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.posted <- b["text"]
		if f.sendReply != "" {
			io.WriteString(w, f.sendReply)
			return
		}
		io.WriteString(w, `{"ok":true,"resumed":false}`)
	})
	mux.HandleFunc("POST /api/chats/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		// Alle Tests, die hier senden, warten mit --wait: dann muss vorher abonniert sein.
		if !f.subscribed.Load() {
			t.Errorf("Befehl gesendet, bevor der Ereignisstrom abonniert war")
		}
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.commands = append(f.commands, b["command"])
		f.mu.Unlock()
		select {
		case f.posted <- b["command"]:
		default:
		}
		io.WriteString(w, `{"ok":true,"resumed":false}`)
	})
	mux.HandleFunc("GET /api/chats/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"compact","description":"Kontext jetzt zusammenfassen","source":"builtin","args":"[Anweisungen]"},{"name":"autocompact","description":"Automatik ein/aus","source":"builtin","args":"on|off"},{"name":"skill:bericht","description":"Bericht schreiben","source":"skill"}]`)
	})
	mux.HandleFunc("POST /api/chats/{id}/autocompact", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]bool
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.autocompact = append(f.autocompact, b["enabled"])
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":"c1","title":"neu","state":"active","auto_compact":%v,"tokens":{"total":0}}`, b["enabled"])
	})
	mux.HandleFunc("POST /api/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]bool
		json.NewDecoder(r.Body).Decode(&b)
		d := decision{r.PathValue("id"), b["approve"]}
		f.mu.Lock()
		f.decisions = append(f.decisions, d)
		f.mu.Unlock()
		f.decided <- d
		fmt.Fprintf(w, `{"id":%q,"chat_id":"c1","state":"approved"}`, d.id)
	})
	mux.HandleFunc("GET /api/approvals", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "pending" {
			t.Errorf("state=pending erwartet: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `[{"id":"a1","chat_id":"c1","name":"x.md","size":3,"via":"cli","state":"pending","created_at":"2026-09-29T10:00:00Z"},{"id":"a2","chat_id":"c2","name":"y.md","size":4,"via":"mcp","state":"pending","created_at":"2026-09-29T10:00:00Z"}]`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) run(args ...string) (code int, stdout, stderr string) {
	return f.runIn("", args...)
}

func (f *fakeServer) runIn(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errw strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env := func(k string) string {
		if k == "AGW_URL" {
			return f.srv.URL
		}
		return ""
	}
	code = realMain(ctx, args, strings.NewReader(stdin), &out, &errw, env)
	return code, out.String(), errw.String()
}

var standardScript = []string{
	`{"kind":"pi","data":{"type":"agent_start"}}`,
	`{"kind":"pi","data":{"type":"message_start","message":{"role":"assistant","content":[]}}}`,
	`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Hallo"}}}`,
	`{"kind":"pi","data":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hallo"}]}}}`,
	`{"kind":"pi","data":{"type":"tool_execution_start","toolCallId":"t1","toolName":"bash","args":{"command":"agw-artifact put bericht.md"}}}`,
	`{"kind":"approval","data":{"id":"a1","chat_id":"c1","kind":"artifact_upload","via":"cli","name":"bericht.md","size":120,"state":"pending"}}`,
	"WAIT_DECISION",
	`{"kind":"approval","data":{"id":"a1","chat_id":"c1","kind":"artifact_upload","via":"cli","name":"bericht.md","size":120,"state":"approved"}}`,
	`{"kind":"pi","data":{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"hochgeladen"}]},"isError":false}}`,
	`{"kind":"pi","data":{"type":"message_start","message":{"role":"assistant","content":[]}}}`,
	`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":" Fertig."}}}`,
	`{"kind":"pi","data":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":" Fertig."}]}}}`,
	`{"kind":"pi","data":{"type":"agent_end"}}`,
	`{"kind":"pi","data":{"type":"agent_settled"}}`,
}

func TestRunAutoApprove(t *testing.T) {
	f := newFakeServer(t)
	f.script = standardScript
	code, out, errw := f.run("run", "--auto-approve", "--variant", "mcp", "--internet=false", "Schreibe einen Bericht")
	if code != 0 {
		t.Fatalf("Exit-Code %d, stderr: %s", code, errw)
	}
	if out != "Hallo\n Fertig.\n" {
		t.Errorf("stdout = %q", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 1 || f.decisions[0] != (decision{"a1", true}) {
		t.Errorf("Entscheidungen = %+v", f.decisions)
	}
	if f.createReq["variant"] != "mcp" || f.createReq["internet"] != false {
		t.Errorf("Anlegen = %v", f.createReq)
	}
	if _, ok := f.createReq["message"]; ok {
		t.Errorf("run darf die Nachricht nicht beim Anlegen senden (Ereignisse gingen verloren): %v", f.createReq)
	}
	for _, want := range []string{`▶ bash {"command":"agw-artifact put bericht.md"}`, "c1", "1500", "0,0042"} {
		if !strings.Contains(errw, want) {
			t.Errorf("stderr ohne %q:\n%s", want, errw)
		}
	}
}

func TestRunInteractiveReject(t *testing.T) {
	f := newFakeServer(t)
	f.script = standardScript
	code, _, errw := f.runIn("n\n", "run", "Aufgabe")
	if code != 0 {
		t.Fatalf("Exit-Code %d, stderr: %s", code, errw)
	}
	if !strings.Contains(errw, "Artefakt bericht.md (120 Bytes) bestätigen? [j/n]") {
		t.Errorf("Frage fehlt: %s", errw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 1 || f.decisions[0].approve {
		t.Errorf("Entscheidungen = %+v", f.decisions)
	}
}

func TestSendWait(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"agent_start"}}`,
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Antwort"}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
	}
	code, out, errw := f.run("chat", "send", "c1", "Hallo", "du", "--wait")
	if code != 0 {
		t.Fatalf("Exit-Code %d: %s", code, errw)
	}
	if !strings.HasPrefix(out, "Antwort") {
		t.Errorf("stdout = %q", out)
	}
}

// Läuft der Agent, reiht der Orchestrator ein; --wait wartet auf den Durchgang nach der
// Übergabe, nicht auf das Ende des laufenden. Schritte beim Fortsetzen erscheinen auf stderr.
func TestSendWaitQueued(t *testing.T) {
	f := newFakeServer(t)
	f.running = true
	f.sendReply = `{"ok":true,"resumed":false,"queued":true,"queue_id":"q1"}`
	f.script = []string{
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"alt "}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
		`{"kind":"queue","data":{"entries":[],"change":"delivered","ids":["q1"],"text":"Hallo du"}}`,
		`{"kind":"pi","data":{"type":"agent_start"}}`,
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Neu"}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
	}
	code, out, errw := f.run("chat", "send", "c1", "Hallo", "du", "--wait")
	if code != 0 {
		t.Fatalf("Exit-Code %d: %s", code, errw)
	}
	if !strings.Contains(out, "Neu") {
		t.Errorf("stdout = %q (Antwort nach der Übergabe fehlt)", out)
	}
	if !strings.Contains(errw, "Eingereiht") || !strings.Contains(errw, "übergeben (1)") {
		t.Errorf("stderr = %q", errw)
	}
}

func TestResumeLine(t *testing.T) {
	size, files := int64(1258291), 14
	cases := []struct {
		in   resumeStep
		want string
	}{
		{resumeStep{Phase: "acquire", Status: "running"}, ""},
		{resumeStep{Phase: "workspace", Status: "done", Size: &size, Files: &files, Ms: 420}, "Fortsetzen: Arbeitsbereich (1,2 MB, 14 Dateien) · 0,4 s"},
		{resumeStep{Phase: "ready", Status: "done", Ms: 3400}, "Chat in einer frischen Sandbox fortgesetzt (3,4 s)."},
		{resumeStep{Phase: "failed", Status: "error", Detail: "Kein freier Platz im Pool"}, "Fortsetzen gescheitert: Kein freier Platz im Pool"},
	}
	for _, c := range cases {
		if got := resumeLine(c.in); got != c.want {
			t.Errorf("resumeLine(%+v) = %q, erwartet %q", c.in, got, c.want)
		}
	}
}

func TestChatNewNoSlot(t *testing.T) {
	f := newFakeServer(t)
	f.noSlot = true
	code, _, errw := f.run("chat", "new", "Hallo")
	if code == 0 {
		t.Fatal("Exit-Code 0 trotz 503")
	}
	if !strings.Contains(errw, "Kein freier Platz im Pool") {
		t.Errorf("stderr = %q", errw)
	}
}

func TestChatNewPrintsID(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "new", "--model", "x/y", "--title", "T", "Los")
	if code != 0 || strings.TrimSpace(out) != "c1" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errw)
	}
	if f.createReq["message"] != "Los" || f.createReq["model"] != "x/y" {
		t.Errorf("Anlegen = %v", f.createReq)
	}
	if _, ok := f.createReq["internet"]; ok {
		t.Errorf("ohne --internet darf das Feld fehlen: %v", f.createReq)
	}
}

func TestModelsTable(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("models")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"Kennung", "deepseek/deepseek-flash", "DeepSeek Flash", "ja", "0,27", "1,10", "0,07", "Spitzentarif"} {
		if !strings.Contains(out, want) {
			t.Errorf("Tabelle ohne %q:\n%s", want, out)
		}
	}
	code, out, _ = f.run("models", "--json")
	var v []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || len(v) != 2 {
		t.Errorf("--json: %d %q", code, out)
	}
}

func TestPoolOutput(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("pool")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"cli", "Ziel 2", "frei 1", "vergeben 1", "mcp", "Ziel 1", "0,0123", "160", "Platz", "Tätigkeit", "p-2", "Führt bash aus", "Test", "1m"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ausgabe ohne %q:\n%s", want, out)
		}
	}
}

func TestChatShow(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "show", "c1")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{"Zähle Dateien", "Ich schaue nach.", `▶ bash {"command":"ls | wc -l"}`, "bericht.md", "daten.csv", "a9"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ausgabe ohne %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hm") {
		t.Errorf("Thinking ohne --thinking: %s", out)
	}
}

func TestApprovalsFilterChat(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("approvals", "--chat", "c1")
	if code != 0 || !strings.Contains(out, "a1") || strings.Contains(out, "a2") {
		t.Errorf("code=%d out=%s", code, out)
	}
}

func TestApproveReject(t *testing.T) {
	f := newFakeServer(t)
	if code, _, e := f.run("approve", "a1"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := f.run("reject", "a2"); code != 0 {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 2 || f.decisions[0] != (decision{"a1", true}) || f.decisions[1] != (decision{"a2", false}) {
		t.Errorf("Entscheidungen = %+v", f.decisions)
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFakeServer(t)
	if code, _, _ := f.run(); code != 2 {
		t.Errorf("ohne Befehl: %d", code)
	}
	if code, _, e := f.run("gibtsnicht"); code != 2 || !strings.Contains(e, "Unbekannter Befehl") {
		t.Errorf("unbekannt: %d %q", code, e)
	}
	if code, _, _ := f.run("chat", "internet", "c1", "vielleicht"); code != 2 {
		t.Errorf("internet ohne on/off: %d", code)
	}
	if code, _, _ := f.run("run", "--auto-approve", "--auto-reject", "x"); code != 2 {
		t.Errorf("beide Auto-Schalter: %d", code)
	}
}

func TestURLFlagOverridesEnv(t *testing.T) {
	f := newFakeServer(t)
	var out, errw strings.Builder
	code := realMain(context.Background(), []string{"--url", f.srv.URL, "models"}, strings.NewReader(""), &out, &errw,
		func(string) string { return "http://127.0.0.1:1" })
	if code != 0 {
		t.Fatalf("%d %s", code, errw.String())
	}
}

func TestDownloadToFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/chats/{id}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "inhalt:"+r.PathValue("name")+":"+r.URL.Query().Get("kind"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "x.md")
	var out, errw strings.Builder
	code := realMain(context.Background(), []string{"chat", "download", "c1", "b.md", "--kind", "input", "-o", dst},
		strings.NewReader(""), &out, &errw, func(k string) string {
			if k == "AGW_URL" {
				return srv.URL
			}
			return ""
		})
	b, _ := os.ReadFile(dst)
	if code != 0 || string(b) != "inhalt:b.md:input" {
		t.Errorf("code=%d datei=%q err=%s", code, b, errw.String())
	}
}

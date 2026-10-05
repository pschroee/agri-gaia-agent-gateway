package agwclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateChatNoSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"pool exhausted"}`)
	}))
	defer srv.Close()
	_, err := New(srv.URL).CreateChat(context.Background(), CreateChatRequest{})
	if !errors.Is(err, ErrNoSlot) {
		t.Fatalf("ErrNoSlot erwartet, erhalten: %v", err)
	}
	if !strings.Contains(err.Error(), "Kein freier Platz im Pool") {
		t.Errorf("Meldung unverständlich: %q", err)
	}
}

func TestCreateChatSendsOptionalFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chats" {
			t.Errorf("falscher Aufruf %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"c1","state":"active","tokens":{"total":0}}`)
	}))
	defer srv.Close()
	off := false
	chat, err := New(srv.URL).CreateChat(context.Background(), CreateChatRequest{Model: "m", Internet: &off})
	if err != nil {
		t.Fatal(err)
	}
	if chat.ID != "c1" {
		t.Errorf("ID falsch: %+v", chat)
	}
	if body["model"] != "m" || body["internet"] != false {
		t.Errorf("Rumpf falsch: %v", body)
	}
	if _, ok := body["variant"]; ok {
		t.Errorf("leere Variante sollte fehlen: %v", body)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"offene Bestätigung"}`)
	}))
	defer srv.Close()
	_, err := New(srv.URL).Suspend(context.Background(), "c1")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 409 || ae.Message != "offene Bestätigung" {
		t.Fatalf("APIError erwartet, erhalten %#v", err)
	}
}

func TestDecide(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"id":"a1","state":"approved"}`)
	}))
	defer srv.Close()
	a, err := New(srv.URL).Decide(context.Background(), "a1", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /api/approvals/a1" || body["approve"] != true || a.State != "approved" {
		t.Errorf("falsch: %s %v %+v", gotPath, body, a)
	}
}

func TestUploadMultipart(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.csv")
	os.WriteFile(p1, []byte("hallo"), 0o644)
	os.WriteFile(p2, []byte("x,y"), 0o644)
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chats/c1/files" {
			t.Errorf("Pfad %s", r.URL.Path)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if part.FormName() != "file" {
				t.Errorf("Feld %q", part.FormName())
			}
			b, _ := io.ReadAll(part)
			got[part.FileName()] = string(b)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `[{"name":"a.txt","kind":"input","size":5},{"name":"b.csv","kind":"input","size":3}]`)
	}))
	defer srv.Close()
	arts, err := New(srv.URL).Upload(context.Background(), "c1", []string{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 || got["a.txt"] != "hallo" || got["b.csv"] != "x,y" {
		t.Errorf("falsch: %+v %v", arts, got)
	}
}

func TestDownloadEscapesNameAndKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/chats/c1/artifacts/mein%20bericht.md" || r.URL.Query().Get("kind") != "input" {
			t.Errorf("falsch: %s ?%s", r.URL.EscapedPath(), r.URL.RawQuery)
		}
		io.WriteString(w, "inhalt")
	}))
	defer srv.Close()
	var sb strings.Builder
	n, err := New(srv.URL).Download(context.Background(), "c1", "mein bericht.md", "input", &sb)
	if err != nil || n != 6 || sb.String() != "inhalt" {
		t.Errorf("falsch: %d %v %q", n, err, sb.String())
	}
}

func TestUnreachable(t *testing.T) {
	_, err := New("http://127.0.0.1:1").Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nicht erreichbar") {
		t.Errorf("verständliche Meldung erwartet: %v", err)
	}
}

func TestCommandsAndRunCommand(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/chats/c1/commands":
			io.WriteString(w, `[{"name":"compact","description":"d","source":"builtin","args":"[Anweisungen]"},{"name":"skill:x","source":"skill"}]`)
		case r.Method == "POST" && r.URL.Path == "/api/chats/c1/commands":
			json.NewDecoder(r.Body).Decode(&got)
			io.WriteString(w, `{"ok":true,"resumed":true,"result":{"a":1}}`)
		case r.Method == "POST" && r.URL.Path == "/api/chats/c1/autocompact":
			var b map[string]bool
			json.NewDecoder(r.Body).Decode(&b)
			if b["enabled"] {
				t.Errorf("enabled=false erwartet: %v", b)
			}
			io.WriteString(w, `{"id":"c1","auto_compact":false,"compactions":3,"context":{"tokens":null,"window":1000,"percent":null,"threshold_tokens":900,"reserve_tokens":100,"keep_recent_tokens":50,"updated_at":"x"}}`)
		default:
			t.Errorf("unerwartet %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()
	cmds, err := c.Commands(ctx, "c1")
	if err != nil || len(cmds) != 2 || cmds[0].Args != "[Anweisungen]" || cmds[1].Source != "skill" {
		t.Fatalf("Commands: %v %+v", err, cmds)
	}
	res, err := c.RunCommand(ctx, "c1", "/compact Fokus")
	if err != nil || !res.OK || !res.Resumed || string(res.Result) != `{"a":1}` {
		t.Fatalf("RunCommand: %v %+v", err, res)
	}
	if got["command"] != "/compact Fokus" {
		t.Errorf("Rumpf = %v", got)
	}
	ch, err := c.SetAutoCompact(ctx, "c1", false)
	if err != nil || ch.AutoCompact || ch.Compactions != 3 || ch.Context == nil || ch.Context.Tokens != nil || ch.Context.Percent != nil || ch.Context.ThresholdTokens != 900 {
		t.Fatalf("SetAutoCompact: %v %+v", err, ch)
	}
}

func TestDecodeModelAndMessageExtras(t *testing.T) {
	var m Model
	if err := json.Unmarshal([]byte(`{"id":"a/b","pricing":{"input":1,"retrieved":"2026-09-28"},"tariff":{"peak_windows_utc":[{"days":"mon-fri","from":"01:00","to":"09:00"}],"offpeak_factor":0.5,"note":"n"},"peak_now":true}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Pricing.Retrieved != "2026-09-28" || m.Tariff == nil || m.Tariff.OffpeakFactor != 0.5 || m.Tariff.PeakWindowsUTC[0].From != "01:00" || m.PeakNow == nil || !*m.PeakNow {
		t.Errorf("Model = %+v", m)
	}
	var sm StoredMessage
	if err := json.Unmarshal([]byte(`{"seq":1,"role":"compaction","message":{},"cost":0.5,"peak":false}`), &sm); err != nil {
		t.Fatal(err)
	}
	if sm.Cost == nil || *sm.Cost != 0.5 || sm.Peak == nil || *sm.Peak {
		t.Errorf("StoredMessage = %+v", sm)
	}
	var cfg Config
	json.Unmarshal([]byte(`{"auto_compact_default":true,"compact_reserve_tokens":16384,"compact_keep_recent_tokens":20000}`), &cfg)
	if !cfg.AutoCompactDefault || cfg.CompactReserveTokens != 16384 || cfg.CompactKeepRecentTokens != 20000 {
		t.Errorf("Config = %+v", cfg)
	}
}

func TestSubagentsAndLLMCalls(t *testing.T) {
	var gotMax map[string]any
	var gotCreate map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chats", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotCreate)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"c1","max_subagents":0,"tokens":{"total":0}}`)
	})
	mux.HandleFunc("POST /api/chats/{id}/subagents", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotMax)
		io.WriteString(w, `{"id":"c1","max_subagents":3,"subagents":1,"llm_calls":4,"cost_other":0.001,"tokens":{"total":0}}`)
	})
	mux.HandleFunc("GET /api/chats/{id}/llm_calls", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":7,"slot_id":"p-1","source_ip":"10.0.0.2","model":"deepseek-flash","response_id":"r1","status":200,"input":100,"output":20,"cache_read":5,"cache_write":0,"cost":0.0003,"peak":true,"tool_calls":[{"name":"bash","arguments":"{}"}],"started_at":"2026-09-29T10:00:00Z","duration_ms":1200,"main":false}]`)
	})
	mux.HandleFunc("GET /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"chat":{"id":"c1","tokens":{"total":0}},"messages":[],"artifacts":[],"approvals":[],"socket_calls":[],"subagent_entries":[{"chat_id":"c1","run_id":"r","entry_id":"e1","agent":"scout","kind":"tool_call","payload":{"name":"bash","arguments":"{\"command\":\"ls\"}"},"response_id":"x","confirmed":true,"created_at":"2026-09-29T10:00:00Z"}]}`)
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"max_subagents_default":2,"max_subagents_limit":8}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()

	zero := 0
	if _, err := c.CreateChat(ctx, CreateChatRequest{MaxSubagents: &zero}); err != nil {
		t.Fatal(err)
	}
	if v, ok := gotCreate["max_subagents"]; !ok || v != float64(0) {
		t.Errorf("max_subagents 0 muss gesendet werden: %v", gotCreate)
	}
	gotCreate = nil
	if _, err := c.CreateChat(ctx, CreateChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotCreate["max_subagents"]; ok {
		t.Errorf("ohne Angabe darf max_subagents fehlen: %v", gotCreate)
	}

	ch, err := c.SetMaxSubagents(ctx, "c1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if gotMax["max"] != float64(3) || ch.MaxSubagents != 3 || ch.Subagents != 1 || ch.LLMCalls != 4 || ch.CostOther != 0.001 {
		t.Errorf("SetMaxSubagents: rumpf=%v chat=%+v", gotMax, ch)
	}

	calls, err := c.LLMCalls(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Input != 100 || !calls[0].Peak || calls[0].Main || len(calls[0].ToolCalls) != 1 || calls[0].ToolCalls[0].Name != "bash" || calls[0].DurationMs != 1200 {
		t.Errorf("LLMCalls = %+v", calls)
	}

	det, err := c.Chat(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(det.SubagentEntries) != 1 || det.SubagentEntries[0].Payload.Name != "bash" || !det.SubagentEntries[0].Confirmed || det.SubagentEntries[0].Agent != "scout" {
		t.Errorf("SubagentEntries = %+v", det.SubagentEntries)
	}

	cfg, err := c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSubagentsDefault != 2 || cfg.MaxSubagentsLimit != 8 {
		t.Errorf("Config = %+v", cfg)
	}
}

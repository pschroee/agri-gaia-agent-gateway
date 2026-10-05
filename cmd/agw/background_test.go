package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agw/internal/agwclient"
)

func TestChatBgAndStop(t *testing.T) {
	var stopped string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/chats/c1/background", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"bg-1","session":"main","command":"sleep 8; echo fertig-bg","state":"exited","exit_code":0,"started_at":"2026-09-29T20:00:00Z","ended_at":"2026-09-29T20:00:08Z","tail":"a\nb\nfertig-bg\n","output_lines":3},
{"id":"bg-2","session":"abc#1","command":"python -m http.server","state":"running","started_at":"2026-09-29T20:00:00Z","tail":""}]`)
	})
	mux.HandleFunc("POST /api/chats/c1/background/{bg}/stop", func(w http.ResponseWriter, r *http.Request) {
		stopped = r.PathValue("bg")
		if stopped == "bg-1" {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":"Hintergrundaufgabe läuft nicht"}`)
			return
		}
		io.WriteString(w, `{"id":"bg-2","state":"stopped","stopped_by":"user","started_at":"2026-09-29T20:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &fakeServer{t: t, srv: srv}
	code, out, errw := f.run("chat", "bg", "c1", "--tail", "2")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{"bg-1  beendet (Exit 0)  0:08  sleep 8; echo fertig-bg", "│ b\n", "│ fertig-bg", "bg-2  läuft", "Subagent abc#1"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ausgabe ohne %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "│ a\n") {
		t.Errorf("mehr als --tail Zeilen:\n%s", out)
	}
	if code, out, _ := f.run("chat", "bg-stop", "c1", "bg-2"); code != 0 || stopped != "bg-2" || !strings.Contains(out, "bg-2: vom Nutzer gestoppt") {
		t.Fatalf("bg-stop: %d %q", code, out)
	}
	if code, _, errw := f.run("chat", "bg-stop", "c1", "bg-1"); code == 0 || !strings.Contains(errw, "läuft nicht") {
		t.Fatalf("bg-stop einer beendeten Aufgabe: %d %q", code, errw)
	}
	if got := bgRuntime(agwclient.BackgroundTask{StartedAt: "2026-09-29T20:00:00Z"}, time.Date(2026, 9, 29, 21, 2, 3, 0, time.UTC)); got != "1:02:03" {
		t.Fatalf("Laufzeit: %s", got)
	}
}

// Review 3 (H2): Der Grund des Zurückhaltens steht in der Anzeige.
func TestHoldReasonText(t *testing.T) {
	for r, want := range map[string]string{"abort": "Abbruch", "wake_limit": "Weckrufe", "auto_turns": "ohne Nutzer", "": ""} {
		if got := holdReasonText(r); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q: %q", r, got)
		}
	}
}

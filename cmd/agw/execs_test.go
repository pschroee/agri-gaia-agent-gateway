package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const execsJSON = `{"summary":{"confirmed":2,"unexecuted":1,"unrequested":1,"mismatch":0,"internal":1},
"calls":[
{"tool_call_id":"call_00_a","state":"confirmed","tool":"bash","executed_tool":"bash","requested":true,"executed":true,"main":true,"session":"main","ops":["bash"],"exit_code":0,"duration_ms":1234,"started_at":"2026-09-29T10:00:01Z","execution_ids":[1]},
{"tool_call_id":"call_01_b","state":"confirmed","tool":"edit","executed_tool":"edit","requested":true,"executed":true,"main":false,"session":"9017da63-d08d-43ab-b978-e86cb7afc45b","ops":["read","write"],"duration_ms":7,"started_at":"2026-09-29T10:00:02Z","execution_ids":[2,3]},
{"tool_call_id":"call_02_c","state":"unexecuted","tool":"read","requested":true,"executed":false,"main":true,"ops":[],"duration_ms":0,"requested_at":"2026-09-29T10:00:03Z","execution_ids":[]},
{"tool_call_id":"call_09_x","state":"unrequested","tool":"bash","executed_tool":"bash","requested":false,"executed":true,"session":"main","ops":["bash"],"error":"aborted","duration_ms":5,"started_at":"2026-09-29T10:00:04Z","execution_ids":[4]},
{"tool_call_id":"call_03_d","state":"internal","tool":"todo","requested":true,"executed":false,"main":true,"ops":[],"duration_ms":0,"execution_ids":[]}],
"executions":[
{"id":1,"session":"main","tool_call_id":"call_00_a","tool":"bash","op":"bash","args":{"command":"python3 script.py","cwd":"/workspace"},"exit_code":0,"output_bytes":3,"started_at":"2026-09-29T10:00:01Z","duration_ms":1234},
{"id":2,"session":"9017da63-d08d-43ab-b978-e86cb7afc45b","tool_call_id":"call_01_b","tool":"edit","op":"read","args":{"path":"/workspace/a.py"},"output_bytes":3,"started_at":"2026-09-29T10:00:02Z","duration_ms":3},
{"id":3,"session":"9017da63-d08d-43ab-b978-e86cb7afc45b","tool_call_id":"call_01_b","tool":"edit","op":"write","args":{"path":"/workspace/a.py","bytes":4},"output_bytes":0,"started_at":"2026-09-29T10:00:02Z","duration_ms":4},
{"id":4,"session":"main","tool_call_id":"call_09_x","tool":"bash","op":"bash","args":{"command":"sleep 99"},"error":"aborted","output_bytes":0,"started_at":"2026-09-29T10:00:04Z","duration_ms":5}]}`

func TestChatExecs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/chats/{id}/tool_executions", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, execsJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	run := func(args ...string) (int, string, string) {
		var out, errw strings.Builder
		code := realMain(context.Background(), args, strings.NewReader(""), &out, &errw, func(k string) string {
			if k == "AGW_URL" {
				return srv.URL
			}
			return ""
		})
		return code, out.String(), errw.String()
	}
	code, out, errw := run("chat", "execs", "c1")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	for _, want := range []string{"Evidence", "confirmed", "NOT EXECUTED", "NOT REQUESTED", "main agent", "Subagent 9017da63", "read, write",
		"python3 script.py", "/workspace/a.py", "aborted", "exit 0", "2 confirmed", "2 suspicious", "call_02_c"} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "todo") {
		t.Errorf("tool without sandbox in the list:\n%s", out)
	}
	code, out, _ = run("chat", "execs", "c1", "--flagged")
	if code != 0 || strings.Contains(out, "python3 script.py") || !strings.Contains(out, "call_09_x") {
		t.Errorf("--flagged: %d\n%s", code, out)
	}
	code, out, _ = run("chat", "execs", "c1", "--json")
	var v map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v["summary"] == nil {
		t.Errorf("--json: %d %q", code, out)
	}
}

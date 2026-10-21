// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/testpg"
)

// --- test helpers ---

// testStore opens a schema of its own; a test without a database skips (as in the other packages).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	st, err := store.OpenSchema(context.Background(), url, testpg.Schema("nbtest"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	return st
}

// testChat creates a chat the runs of the tests belong to (notebook_runs references chats).
func testChat(t *testing.T, st *store.Store) string {
	t.Helper()
	c, err := st.CreateChat(context.Background(), store.NewChat{Title: "notebook test", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// fakeHub is a JupyterHub API with the endpoints the gateway uses.
type fakeHub struct {
	mu       sync.Mutex
	requests []string
	// server: the server name the hub reports a state for.
	server string
	// userKnown: the hub knows the chat's user (the first OAuth login happened).
	userKnown bool
	// ready: the server of the user answers.
	ready bool
	// startCode/startBody answer the start (0: 201).
	startCode int
	startBody string
	// startRaces: the refused start (startCode) leaves a ready server behind, as if another start won the race.
	startRaces bool
	// reachable false: every call fails as if the hub were down.
	reachable bool
	stopped   int
	// pending "stop": the hub is stopping the server; a start answers 400 "pending stop" and the user
	// model carries "pending". pendingPosts counts the starts that are refused before the stop ends
	// (-1: never).
	pending      string
	pendingPosts int
	// pendingReads: that many reads of the user model still report pending "stop"; after the last one
	// the hub has stopped the server (no entry, not pending).
	pendingReads int
	// gate, when set, holds every read of the user model until it is closed.
	gate chan struct{}
}

func newFakeHub() *fakeHub { return &fakeHub{userKnown: true, ready: true, reachable: true} }

func (h *fakeHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		reachable := h.reachable
		known, ready, server := h.userKnown, h.ready, h.server
		startCode, startBody := h.startCode, h.startBody
		gate := h.gate
		h.mu.Unlock()
		if !reachable {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/servers/"):
			h.mu.Lock()
			if h.pending == "stop" {
				if h.pendingPosts != 0 {
					if h.pendingPosts > 0 {
						h.pendingPosts--
					}
					h.mu.Unlock()
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":400,"message":"` + lastSegment(r.URL.Path) + ` is pending stop"}`))
					return
				}
				h.pending = ""
			}
			if startCode == 0 && h.server != "" && h.server == lastSegment(r.URL.Path) && h.ready {
				h.mu.Unlock()
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"message":"agent-test:` + h.server + ` is already running"}`))
				return
			}
			h.mu.Unlock()
			if startCode != 0 {
				if h.startRaces {
					h.mu.Lock()
					h.server, h.ready = lastSegment(r.URL.Path), true
					h.mu.Unlock()
				}
				w.WriteHeader(startCode)
				_, _ = w.Write([]byte(startBody))
				return
			}
			h.mu.Lock()
			h.server = lastSegment(r.URL.Path)
			h.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/servers/"):
			h.mu.Lock()
			if h.server == "" {
				h.mu.Unlock()
				w.WriteHeader(http.StatusNotFound) // a server that is not there is not an error for the gateway
				return
			}
			h.stopped++
			h.ready, h.server = false, ""
			h.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/users/"):
			if gate != nil {
				<-gate
			}
			h.mu.Lock()
			pending := h.pending
			if h.pendingReads > 0 {
				h.pendingReads--
				if h.pendingReads == 0 {
					defer func() {
						h.mu.Lock()
						h.server, h.pending = "", "" // ready stays: the next start comes up ready
						h.mu.Unlock()
					}()
				}
			}
			h.mu.Unlock()
			if !known {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"No such user"}`))
				return
			}
			servers := "{}"
			if server != "" {
				servers = fmt.Sprintf(`{"%s":{"ready":%t,"pending":%q}}`, server, ready, pending)
			}
			fmt.Fprintf(w, `{"name":%q,"servers":%s}`, lastSegment(r.URL.Path), servers)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// lastSegment is the last part of a URL path (the user or server name of a hub request).
func lastSegment(p string) string {
	p = strings.TrimRight(p, "/")
	return p[strings.LastIndex(p, "/")+1:]
}

// deleted lists the servers the gateway asked the hub to stop, in order.
func (h *fakeHub) deleted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.requests {
		if !strings.HasPrefix(r, http.MethodDelete+" ") {
			continue
		}
		if i := strings.Index(r, "/servers/"); i >= 0 {
			out = append(out, r[i+len("/servers/"):])
		}
	}
	return out
}

// userToken returns a JWT whose preferred_username is the given name: the platform client reads the
// user of a chat out of the token, and the notebook service asks it for the hub user.
func userToken(name string) string {
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]any{"preferred_username": name, "sub": "u1", "exp": 4102444800})
	return enc.EncodeToString([]byte("{}")) + "." + enc.EncodeToString(claims) + ".s"
}

// platformWithUser builds a platform client that names the given user (no token endpoint needed).
func platformWithUser(t *testing.T, apiURL, name string) *platform.Client {
	t.Helper()
	c, err := platform.New(platform.Config{APIURL: apiURL,
		Subject: func(context.Context, string) (string, error) { return userToken(name), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// --- the hub ---

// The server name has to start with "agw-": the hub's spawner hardens only servers with that prefix and
// runs everything else with the user's privileges in the default image (plan E2). So the built-in
// fallback has to carry it too (AGW_NOTEBOOK_PREFIX is only the override).
func TestServerNameCarriesHardenedPrefix(t *testing.T) {
	n := &Notebooks{}
	if got := n.Server("2d44c5f1-b7be-4620-b0be-e758c0e6c6b6"); !strings.HasPrefix(got, "agw-") {
		t.Fatalf("server name %q does not start with agw-", got)
	}
	if got := n.Server("2d44c5f1-b7be-4620-b0be-e758c0e6c6b6"); got != "agw-2d44c5f1" {
		t.Fatalf("server name %q", got)
	}
}

// Start, Ready and Stop speak the hub's API: the paths, the token header and the answers.
func TestHubStartReadyStop(t *testing.T) {
	hub := newFakeHub()
	srv := httptest.NewServer(hub.handler())
	defer srv.Close()
	h := NewHub(srv.URL, "tok", nil)
	ctx := context.Background()
	const server = "agri_gaia-jupyter-1a2b3c4d"

	// Before the first start the hub knows the user, but there is no server.
	state, err := h.ServerState(ctx, "agent-test", server)
	if err != nil || state != "not started" {
		t.Fatalf("state before the start: %q %v", state, err)
	}
	if err := h.Start(ctx, "agent-test", server); err != nil {
		t.Fatalf("start: %v", err)
	}
	ready, err := h.Ready(ctx, "agent-test", server)
	if err != nil || !ready {
		t.Fatalf("ready: %v %v", ready, err)
	}
	if state, err := h.ServerState(ctx, "agent-test", server); err != nil || state != "running" {
		t.Fatalf("state while running: %q %v", state, err)
	}
	hub.mu.Lock()
	hub.ready = false
	hub.mu.Unlock()
	if state, err := h.ServerState(ctx, "agent-test", server); err != nil || state != "starting" {
		t.Fatalf("state while starting: %q %v", state, err)
	}
	hub.mu.Lock()
	hub.ready = true
	hub.mu.Unlock()
	if known, err := h.HasUser(ctx, "agent-test"); err != nil || !known {
		t.Fatalf("HasUser: %v %v", known, err)
	}
	if err := h.Stop(ctx, "agent-test", server); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Stopping a server that is not there counts as stopped.
	if err := h.Stop(ctx, "agent-test", server); err != nil {
		t.Fatalf("stop again: %v", err)
	}
	if hub.stopped != 1 {
		t.Errorf("stops seen by the hub: %d (the second call must not reach it as a server)", hub.stopped)
	}
	hub.mu.Lock()
	reqs := append([]string(nil), hub.requests...)
	hub.mu.Unlock()
	want := []string{
		"GET /users/agent-test",
		"POST /users/agent-test/servers/agri_gaia-jupyter-1a2b3c4d",
		"GET /users/agent-test",
		"GET /users/agent-test",
		"GET /users/agent-test",
		"GET /users/agent-test",
		"DELETE /users/agent-test/servers/agri_gaia-jupyter-1a2b3c4d",
		"DELETE /users/agent-test/servers/agri_gaia-jupyter-1a2b3c4d",
	}
	if strings.Join(reqs, " | ") != strings.Join(want, " | ") {
		t.Errorf("hub calls:\n got  %v\n want %v", reqs, want)
	}
	// A started server that is not ready yet takes its time; a server the hub already runs is no error.
	hub.mu.Lock()
	hub.startCode, hub.startBody = http.StatusBadRequest, `{"message":"This user's server is already running."}`
	hub.mu.Unlock()
	if err := h.Start(ctx, "agent-test", server); err != nil {
		t.Errorf("an already running server counts as started: %v", err)
	}

	// A hub that does not know the user: the state call says ErrNoHubUser (the gateway has no
	// admin:users scope and asks the user to open JupyterHub once instead).
	hub.mu.Lock()
	hub.userKnown = false
	hub.mu.Unlock()
	if _, err := h.Ready(ctx, "agent-test", server); !errors.Is(err, ErrNoHubUser) {
		t.Errorf("ready without the user: %v", err)
	}
	if _, err := h.ServerState(ctx, "agent-test", server); !errors.Is(err, ErrNoHubUser) {
		t.Errorf("state without the user: %v", err)
	}
	if known, err := h.HasUser(ctx, "agent-test"); err != nil || known {
		t.Errorf("HasUser: %v %v", known, err)
	}
	// A hub that is not there fails with ErrHub, so that a tool can answer "unavailable".
	hub.mu.Lock()
	hub.reachable = true
	hub.ready, hub.server = true, server
	hub.mu.Unlock()
	dead := NewHub("http://127.0.0.1:1", "tok", nil)
	if err := dead.Start(ctx, "agent-test", server); !errors.Is(err, ErrHub) {
		t.Errorf("unreachable hub: %v", err)
	}
}

// Every hub call checks the names: an invalid user or server never reaches a URL.
func TestHubRejectsBadUser(t *testing.T) {
	h := NewHub("http://127.0.0.1:1/hub/api", "tok", nil)
	ctx := context.Background()
	for _, user := range []string{"Agent-Test", "../admin", "", "ä", strings.Repeat("a", 64)} {
		if err := h.Start(ctx, user, "agri_gaia-jupyter-1a2b3c4d"); err == nil {
			t.Errorf("user %q accepted", user)
		}
		if _, err := h.Ready(ctx, user, "agri_gaia-jupyter-1a2b3c4d"); err == nil {
			t.Errorf("user %q accepted by ready", user)
		}
		if err := h.Stop(ctx, user, "agri_gaia-jupyter-1a2b3c4d"); err == nil {
			t.Errorf("user %q accepted by stop", user)
		}
		if _, err := h.HasUser(ctx, user); err == nil {
			t.Errorf("user %q accepted by HasUser", user)
		}
	}
	for _, server := range []string{"agri_gaia-jupyter-1a2b3c4d/../x", "a b", ""} {
		if err := h.Start(ctx, "agent-test", server); err == nil {
			t.Errorf("server %q accepted", server)
		}
	}
}

// --- the service, without a hub ---

// Without a configuration every notebook tool answers "unavailable" instead of an error (G1.13).
func TestNotebookToolsUnavailable(t *testing.T) {
	n := New(Config{}, nil, nil, nil)
	res, err := n.Do(context.Background(), "chat", platform.Request{Method: http.MethodGet, Path: platform.NotebookPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != platform.StatusUnavailable || !strings.Contains(res.Message, "not configured on this instance") {
		t.Fatalf("answer: %+v", res)
	}
	if !strings.Contains(res.Message, "capabilities shows what this setup offers") {
		t.Errorf("answer without the way to the overview: %q", res.Message)
	}
}

// The capability entry reports why notebooks are not available, in the four states of the plan.
func TestNotebookCapability(t *testing.T) {
	ctx := context.Background()
	// 1. not configured
	n := New(Config{}, nil, nil, nil)
	if avail, detail := n.NotebookState(ctx, "chat"); avail || !strings.Contains(detail, "not configured on this instance") {
		t.Errorf("not configured: %v %q", avail, detail)
	}

	hub := newFakeHub()
	srv := httptest.NewServer(hub.handler())
	defer srv.Close()
	st := testStore(t)
	chatID := testChat(t, st)
	n = New(Config{APIURL: srv.URL + "/hub/api", Token: "tok", PublicURL: "http://hub.example", Prefix: "agri_gaia-jupyter"}, platformWithUser(t, srv.URL, "agent-test"), st, nil)

	// 2. the hub is not reachable
	hub.mu.Lock()
	hub.reachable = false
	hub.mu.Unlock()
	if avail, detail := n.NotebookState(ctx, chatID); avail || !strings.Contains(detail, "JupyterHub not reachable") {
		t.Errorf("unreachable: %v %q", avail, detail)
	}

	// 3. the hub does not know the user
	hub.mu.Lock()
	hub.reachable, hub.userKnown = true, false
	hub.mu.Unlock()
	if avail, detail := n.NotebookState(ctx, chatID); avail || !strings.Contains(detail, "open JupyterHub once in the browser") {
		t.Errorf("unknown user: %v %q", avail, detail)
	}

	// 4. available, with the state of the server
	hub.mu.Lock()
	hub.userKnown, hub.ready = true, false
	hub.mu.Unlock()
	if avail, detail := n.NotebookState(ctx, chatID); !avail || !strings.Contains(detail, "not started") {
		t.Errorf("available: %v %q", avail, detail)
	}
	// A running server: the hub lists it in the user's servers (ServerState reads it from there).
	hub.mu.Lock()
	hub.ready, hub.server = true, n.Server(chatID)
	hub.mu.Unlock()
	avail, detail := n.NotebookState(ctx, chatID)
	if !avail || !strings.Contains(detail, n.Server(chatID)) || !strings.Contains(detail, "no internet") || !strings.Contains(detail, "running") {
		t.Errorf("running server: %v %q", avail, detail)
	}
	if n.Server(chatID) != "agri_gaia-jupyter-"+strings.ReplaceAll(chatID, "-", "")[:8] {
		t.Errorf("server name: %q for chat %q", n.Server(chatID), chatID)
	}
}

// The run script quotes what the agent named, and both endings start the entry the right way.
func TestRunScriptQuoting(t *testing.T) {
	script := runScript("r20260101120000-abcd", "train.py", []string{"--epochs", "5", "a-b=c:/d+e"}, 120)
	for _, w := range []string{
		"cd '/home/jovyan/work/runs/r20260101120000-abcd' || exit 99",
		"timeout --signal=TERM --kill-after=30s 120m python -u 'train.py' '--epochs' '5' 'a-b=c:/d+e' > run.log 2>&1",
		"echo $rc > .agw/exit_code",
		"date -u +%FT%TZ > .agw/started",
		"date -u +%FT%TZ > .agw/finished",
	} {
		if !strings.Contains(script, w) {
			t.Errorf("script without %q:\n%s", w, script)
		}
	}
	// Nothing of the agent's input reaches the script unquoted: every value stands in single quotes.
	if strings.Count(script, "'")%2 != 0 {
		t.Errorf("unbalanced quotes in the script:\n%s", script)
	}
	nb := runScript("r20260101120000-abcd", "notebook.ipynb", nil, 30)
	if !strings.Contains(nb, "python -u .agw/agw_run_notebook.py 'notebook.ipynb' --out 'executed-notebook.ipynb' --markers .agw") {
		t.Errorf("notebook script:\n%s", nb)
	}
	if !strings.Contains(nb, "30m") || strings.Contains(nb, "nbconvert") || strings.Contains(nb, "python -u 'notebook.ipynb'") {
		t.Errorf("notebook script runs the wrong command:\n%s", nb)
	}
}

// The arguments and the files of a run are checked before anything is started.
func TestRunValidation(t *testing.T) {
	entry := map[string][]string{"entry": {"train.py"}, "timeout_minutes": {"60"}}
	if _, err := platform.ParseRunForm(entry, 0); err != nil {
		t.Fatalf("the known form must pass: %v", err)
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"train.py"}, "shell": {"rm -rf /"}}, 0); err == nil {
		t.Error("an unknown field must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"a.py", "b.py"}}, 0); err == nil {
		t.Error("a repeated entry must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"/etc/passwd"}}, 0); err == nil {
		t.Error("an entry with a slash must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"train.py"}, "args": {"a; rm -rf /"}}, 0); err == nil {
		t.Error("an argument with a shell character must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"train.py"}, "timeout_minutes": {"481"}}, 0); err == nil {
		t.Error("a timeout above the ceiling must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"train.py"}, "timeout_minutes": {"60"}}, 30); err == nil {
		t.Error("a timeout above the configured ceiling must be refused")
	}
	tooMany := map[string][]string{"entry": {"train.py"}, "args": {}}
	for i := 0; i < 21; i++ {
		tooMany["args"] = append(tooMany["args"], fmt.Sprintf("--a%d", i))
	}
	if _, err := platform.ParseRunForm(tooMany, 0); err == nil {
		t.Error("more than 20 arguments must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"train.py"}, "dataset_ids": {"1", "2", "3", "4"}}, 0); err == nil {
		t.Error("more than 3 datasets must be refused")
	}
	if _, err := platform.ParseRunForm(map[string][]string{"entry": {"train.py"}, "dataset_ids": {"x"}}, 0); err == nil {
		t.Error("a dataset id that is not a number must be refused")
	}

	body := platform.RunBody{Entry: "train.py", TimeoutMinutes: 60}
	if err := platform.ValidateRunFiles(body, []string{"train.py"}, nil); err != nil {
		t.Fatalf("one allowed file: %v", err)
	}
	if err := platform.ValidateRunFiles(body, nil, nil); err == nil {
		t.Error("a run without files must be refused")
	}
	if err := platform.ValidateRunFiles(platform.RunBody{Entry: "data.pt"}, []string{"data.pt"}, nil); err == nil {
		t.Error("an ending outside the list must be refused")
	}
	if err := platform.ValidateRunFiles(body, []string{"a.py", "a.py"}, nil); err == nil {
		t.Error("two files of the same name must be refused")
	}
	if err := platform.ValidateRunFiles(body, []string{"other.py"}, nil); err == nil {
		t.Error("an entry that is not among the files must be refused")
	}
	if err := platform.ValidateRunFiles(body, []string{"train.py"}, []int64{platform.NotebookFileMaxBytes + 1}); err == nil {
		t.Error("a file above the single limit must be refused")
	}
	if err := platform.ValidateRunFiles(body, []string{"train.py"}, []int64{platform.NotebookFilesMax + 1}); err == nil {
		t.Error("files above the total limit must be refused")
	}
	files := make([]string, platform.NotebookMaxFiles+1)
	for i := range files {
		files[i] = fmt.Sprintf("f%d.py", i)
	}
	if err := platform.ValidateRunFiles(platform.RunBody{Entry: files[0]}, files, nil); err == nil {
		t.Error("more than 50 files must be refused")
	}
}

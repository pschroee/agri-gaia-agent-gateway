package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/platform"
	"agw/internal/sock"
	"agw/internal/store"
)

var _ sock.PlatformBackend = (*Manager)(nil)

// withPlatform attaches a fake platform to the manager and returns the calls
// that arrived there.
func withPlatform(t *testing.T, e *env) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Location", "/tasks/5")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestPlatformCallReadDirect(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "mcp", platform.Request{Method: "GET", Path: "/datasets"})
	if err != nil || r.Status != "ok" || len(got()) != 1 {
		t.Fatalf("reading: %+v %v %v", r, err, got())
	}
	if aps, _ := e.st.ListApprovals(ctx, "", c.ID); len(aps) != 0 {
		t.Fatalf("GET must not create an approval: %+v", aps)
	}
}

func TestPlatformCallWriteNeedsApproval(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req, _ := platform.Normalize(platform.Request{Method: "POST", Path: "/train/config", Body: json.RawMessage(`{"dataset_id":2}`)})
	for _, approve := range []bool{false, true} {
		done := make(chan platform.Result, 1)
		go func() {
			r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
			done <- r
		}()
		ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
		for ap.State != store.ApprovalPending { // skip the event of the previous decision
			ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
		}
		if ap.Kind != "platform_write" || ap.Name != "POST /train/config" || !strings.Contains(ap.Preview, `"dataset_id": 2`) {
			t.Fatalf("approval: %+v", ap)
		}
		if n := len(got()); n != 0 && !approve {
			t.Fatalf("executed before the decision: %v", got())
		}
		if _, err := e.m.Decide(ctx, ap.ID, approve); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if !approve && (r.Status != "rejected" || len(got()) != 0) {
			t.Fatalf("rejection: %+v %v", r, got())
		}
		if approve && (r.Status != "ok" || r.HTTPStatus != 202 || r.Location != "/tasks/5" || len(got()) != 1) {
			t.Fatalf("approval granted: %+v %v", r, got())
		}
	}
}

func TestPlatformCallNotConfigured(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	r, err := e.m.PlatformCall(ctx, c.ID, "", "cli", platform.Request{Method: "POST", Path: "/x"})
	if err != nil || r.Status != "error" {
		t.Fatalf("without platform: %+v %v", r, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, "", c.ID); len(aps) != 0 {
		t.Fatal("no approval without platform")
	}
}

// Review W1: what does not fit completely into the preview is refused, not approved truncated.
func TestPlatformCallPreviewTooLarge(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	body, _ := json.Marshal(map[string]string{"description": strings.Repeat("x", maxPlatformPreview)})
	r, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "mcp", platform.Request{Method: "PATCH", Path: "/datasets/1", Body: body})
	if err != nil || r.Status != "error" || !strings.Contains(r.Message, "too large") || len(got()) != 0 {
		t.Fatalf("preview too large: %+v %v %v", r, err, got())
	}
	if aps, _ := e.st.ListApprovals(ctx, "", c.ID); len(aps) != 0 {
		t.Fatalf("no approval expected: %+v", aps)
	}
}

// Review M7: expiry of the waiting time executes nothing; a writing GET (K1) asks for approval;
// control characters appear visibly in the preview.
func TestPlatformCallExpiredAndWritingGET(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "GET", Path: "/train/containers/3/model"})
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if ap.Kind != "platform_write" || ap.Name != "GET /train/containers/3/model" {
		t.Fatalf("writing GET without approval: %+v", ap)
	}
	if r := <-done; r.Status != "rejected" || len(got()) != 0 { // ApprovalTimeout in the test: 2 s
		t.Fatalf("expiry: %+v %v", r, got())
	}
	rlo := string(rune(0x202e)) // bidi control character RIGHT-TO-LEFT OVERRIDE
	if s := visibleControls("a" + rlo + "b\nc"); s != "a\\u202eb\nc" {
		t.Fatalf("control characters: %q", s)
	}
}

// An upload shows every file with size and SHA-256 in the approval.
func TestPlatformCallUploadPreview(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req := platform.Request{Method: "POST", Path: "/models", Form: map[string][]string{"name": {"m"}, "format": {"onnx"}},
		Files:   []platform.File{{Field: "modelfile", Path: "/workspace/m.onnx"}},
		Uploads: []platform.Upload{{Field: "modelfile", Name: "m.onnx", Data: []byte("12345"), SHA256: "abc123"}}}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "mcp", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if ap.Name != "POST /models (multipart, 1 file)" || ap.Size != 5 || !strings.Contains(ap.Preview, "modelfile: m.onnx  5 bytes  sha256 abc123") || !strings.Contains(ap.Preview, "format = onnx") {
		t.Fatalf("approval: %+v", ap)
	}
	_, _ = e.m.Decide(ctx, ap.ID, true)
	if r := <-done; r.Status != "ok" || len(got()) != 1 {
		t.Fatalf("upload after approval: %+v %v", r, got())
	}
}

// With token exchange, every exchange is in the chat's socket log.
func TestPlatformExchangeLogged(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	enc := base64.RawURLEncoding
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "password" {
				fmt.Fprint(w, `{"access_token":"user","expires_in":3600}`)
				return
			}
			claims, _ := json.Marshal(map[string]any{"preferred_username": "test", "azp": "agw-agent", "aud": r.Form["audience"], "exp": 4102444800})
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, enc.EncodeToString([]byte("{}"))+"."+enc.EncodeToString(claims)+".s")
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	pc, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "agw-agent", ClientSecret: "s", User: "u", Password: "p", Exchange: true})
	if err != nil {
		t.Fatal(err)
	}
	pc.SetOnExchange(e.m.PlatformExchanged)
	e.m.opt.Platform = pc
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := e.m.PlatformCall(ctx, c.ID, slot, "mcp", platform.Request{Method: "GET", Path: "/datasets"}); r.Status != "ok" {
		t.Fatalf("call: %+v", r)
	}
	calls, _ := e.st.ListSocketCalls(ctx, c.ID)
	found := false
	for _, sc := range calls {
		if sc.Op == "token_exchange" && sc.Via == "orchestrator" && strings.Contains(sc.Detail, "azp=agw-agent, aud=backend,minio") && sc.SlotID == slot {
			found = true
		}
	}
	if !found {
		t.Fatalf("exchange not logged: %+v", calls)
	}
}

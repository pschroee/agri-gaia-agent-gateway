package worker

// Slot test of the web search: real pi container with pi-searxng-suite and web-gate.ts, the
// web proxy runs in the test process (alias "orchestrator" in the slot network), a test server plays
// SearXNG and the fetched page. Without internet pi does not offer the tools; with internet, search
// and fetch go through the proxy and are logged.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/sandbox"
	"agw/internal/store"
	"agw/internal/webproxy"
)

type webBackend struct {
	bgBackend
	internet atomic.Bool
	wmu      sync.Mutex
	web      []webproxy.Request
}

func (b *webBackend) InternetOn(context.Context, string) bool { return b.internet.Load() }
func (b *webBackend) WebAccess(string) (string, string, bool) {
	return "chat-e9", "p", b.internet.Load()
}
func (b *webBackend) RecordWeb(r webproxy.Request) int64 {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	b.web = append(b.web, r)
	return int64(len(b.web))
}
func (b *webBackend) FinishWeb(int64, webproxy.Request) {}

func TestSlotWebSearch(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("runs only in the Go container with the Docker socket (./dev.sh test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fake := useFakeLLM()
	// SearXNG and target page in the test process.
	var qmu sync.Mutex
	var queries []string
	site := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			qmu.Lock()
			queries = append(queries, r.URL.Query().Get("q"))
			qmu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"results":[{"title":"Result for %s","url":"http://target.test/page","content":"Short text","engine":"test"}]}`, r.URL.Query().Get("q"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><title>Tail biting</title></head><body><article><h1>Detect it early</h1><p>The pigs are restless and manipulate objects in the pen.</p></article></body></html>`)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go site.Serve(ln)
	defer site.Close()
	sitePort := ln.Addr().(*net.TCPAddr).Port
	b := &webBackend{bgBackend: bgBackend{ended: map[string]store.BackgroundTask{}, notify: map[string]bool{}}}
	su, _ := url.Parse("http://" + ln.Addr().String())
	proxy := &webproxy.Proxy{Gate: b, Searx: su, HTTPPort: sitePort, Allow: func(ip net.IP) bool { return ip.IsLoopback() },
		Lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host == "target.test" {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}}
	psrv := &http.Server{Addr: ":18486", Handler: proxy}
	go psrv.ListenAndServe()
	defer psrv.Close()

	rt, err := sandbox.New(envOr("AGW_EGRESS_NETWORK", "agwpoc_egress"))
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Hostname()
	rt.SetSelf(self)
	cat, err := config.LoadCatalog(envOr("AGW_CATALOG", "../../models.json"))
	if err != nil {
		t.Fatal(err)
	}
	env := config.Env{
		Image: envOr("AGW_IMAGE", "agwpoc/agw-basis:dev"), PiImage: envOr("AGW_PI_IMAGE", "agwpoc/agw-pi:dev"),
		SocketVolume: envOr("AGW_SOCKET_VOLUME", "agwpoc_sockets"), SocketRoot: envOr("AGW_SOCKET_ROOT", "/run/agw"),
		ProxyBaseURL: "http://orchestrator:18481", WebProxyURL: "http://orchestrator:18486", ArtifactMaxBytes: 1 << 20,
		CompactReserveTokens: 16384, CompactKeepRecent: 20000,
		SandboxMemoryMB: 1536, SandboxCPUs: 1, SandboxPids: 256, ExecMemoryMB: 1024, ExecCPUs: 1, ExecPids: 256, BgMax: 3,
	}
	fac, err := NewFactory(rt, cat, env)
	if err != nil {
		t.Fatal(err)
	}
	fac.Backend = b
	slot := fmt.Sprintf("t-web-%d", time.Now().UnixNano()%1e8)
	a, err := fac.Create(ctx, slot, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer fac.Destroy(context.Background(), a)
	w := a.(*Worker)

	type end struct {
		isError bool
		text    string
	}
	var mu sync.Mutex
	ends := map[string]end{}
	settled := make(chan struct{}, 4)
	go func() {
		for ev := range w.Events() {
			switch ev.Type {
			case "tool_execution_start":
				// The user allows internet in the middle of the turn: the tools are added before the
				// next model call (web-gate.ts, turn_start).
				if strings.Contains(string(ev.Raw), "switch-on") {
					b.internet.Store(true)
				}
			case "tool_execution_end":
				var e struct {
					ToolCallID string `json:"toolCallId"`
					IsError    bool   `json:"isError"`
					Result     struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				_ = json.Unmarshal(ev.Raw, &e)
				var txt strings.Builder
				for _, c := range e.Result.Content {
					txt.WriteString(c.Text)
				}
				mu.Lock()
				ends[e.ToolCallID] = end{e.IsError, txt.String()}
				mu.Unlock()
			case "agent_settled":
				settled <- struct{}{}
			}
		}
	}()
	run := func(lines ...string) {
		if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": strings.Join(lines, "\n")}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-settled:
		case <-ctx.Done():
			t.Fatal("run does not finish")
		}
	}
	result := func(tool, contains string) end {
		for _, is := range fake.Issued() {
			if is.Tool == tool && strings.Contains(is.Args, contains) {
				mu.Lock()
				defer mu.Unlock()
				return ends[is.ID]
			}
		}
		t.Fatalf("not requested: %s %s", tool, contains)
		return end{}
	}
	// First without internet (the tool does not exist), then the user switches it on, and search and
	// fetch go through the proxy.
	run("Web search.",
		callLine("web_search", map[string]any{"query": "without"}),
		callLine("bash", map[string]any{"command": "echo switch-on"}),
		callLine("web_search", map[string]any{"query": "tailbiting"}),
		callLine("web_extract", map[string]any{"url": "http://target.test:" + strconv.Itoa(sitePort) + "/page"}),
		// Subagents may do what the main agent may do: the researcher also searches through the same proxy.
		callLine("subagent", map[string]any{"agent": "researcher", "async": false,
			"task": "R1\n" + callLine("web_search", map[string]any{"query": "subagentsearch"})}))
	if r := result("web_search", "without"); !r.isError || !strings.Contains(strings.ToLower(r.text), "not found") {
		t.Errorf("without internet: %+v", r)
	}
	if r := result("web_search", "tailbiting"); r.isError || !strings.Contains(r.text, "Result for tailbiting") {
		t.Errorf("web_search: %+v", r)
	}
	if r := result("web_extract", "target.test"); r.isError || !strings.Contains(r.text, "manipulate") {
		t.Errorf("web_extract: %+v", r)
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	var hosts []string
	for _, r := range b.web {
		hosts = append(hosts, fmt.Sprintf("%s %s %d %s", r.Method, r.Host, r.Status, r.Denied))
	}
	if len(b.web) < 2 || b.web[0].Host != "searxng" || b.web[1].Host != "target.test" || b.web[0].Status != 200 || b.web[1].Status != 200 {
		t.Errorf("proxy log: %v", hosts)
	}
	// The child session runs in the same Node process and shares the open tunnel: its search is
	// therefore proven at the simulated SearXNG, not by a second CONNECT.
	qmu.Lock()
	got := strings.Join(queries, ",")
	qmu.Unlock()
	if !strings.Contains(got, "subagentsearch") {
		t.Errorf("the subagent's search did not reach SearXNG: %q", got)
	}
}

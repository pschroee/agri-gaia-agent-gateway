package worker

// Parity of the redirection (exec-bridge.ts) with pi's built-in tools (code review:
// test gap "parity of the bridge tools against pi", M2, L1, L3, L4, H1). A container from
// the test image agw-parity is both the execution sandbox and the place where pi's tools run:
// pi works directly on its file system, the bridge via this test's socket, whose
// tool endpoints (sock.NewPiHandler) execute with agw-exec serve in the same container.
// The script images/agw-basis/test/parity.mjs compares per case the text, the details and, for long
// bash output, the content of the file with the full output.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agw/internal/execbox"
	"agw/internal/execproto"
	"agw/internal/sandbox"
	"agw/internal/sock"
)

type parityResult struct {
	Name      string          `json:"name"`
	Equal     bool            `json:"equal"`
	Builtin   json.RawMessage `json:"builtin"`
	Bridge    json.RawMessage `json:"bridge"`
	FullEqual bool            `json:"fullEqual"`
	FullSaved bool            `json:"fullSaved"`
}

func TestBridgeParity(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("runs only in the Go container with the Docker socket (./dev.sh test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	rt, err := sandbox.New(envOr("AGW_EGRESS_NETWORK", "agwpoc_egress"))
	if err != nil {
		t.Fatal(err)
	}
	slot := fmt.Sprintf("t-parity-%d", time.Now().UnixNano()%1e8)
	socketRoot := envOr("AGW_SOCKET_ROOT", "/run/agw")
	xnet, err := rt.CreateExecNetwork(ctx, slot)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.RemoveSlotNetwork(context.Background(), xnet)
	var inst *sandbox.Instance
	box := execbox.New(func(dctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
		in, out, closeFn, _, err := rt.ExecStream(context.WithoutCancel(dctx), inst.ID, []string{"/usr/local/bin/agw-exec", "serve"}, "0:0")
		return in, out, closeFn, err
	})
	defer box.Close()
	b := &e9Backend{}
	dir := filepath.Join(socketRoot, slot)
	srv, err := sock.Listen(filepath.Join(dir, "pi"), sock.NewPiHandler(slot, b, 1<<20, box, b))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); os.RemoveAll(dir) }()
	inst, err = rt.Start(ctx, sandbox.Spec{
		Name: "agwpoc-" + slot, Image: envOr("AGW_PARITY_IMAGE", "agwpoc/agw-parity:dev"), NoAttach: true,
		Labels: map[string]string{sandbox.LabelManaged: "test", sandbox.LabelSlot: slot}, Tmpfs: sandbox.ExecTmpfs,
		CapAdd: sandbox.ExecCaps, InternalNet: xnet,
		SocketVolume: envOr("AGW_SOCKET_VOLUME", "agwpoc_sockets"), SocketSubpath: slot + "/pi",
		MemoryMB: 3072, CPUs: 2, Pids: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)
	if f, err := box.Run(ctx, execproto.Request{Op: execproto.OpStat, Path: "/workspace"}, nil); err != nil || f.Error != "" {
		t.Fatalf("supervisor: %v %s", err, f.Error)
	}
	cmd := []string{"node", "/opt/pi/test/parity.mjs"}
	if only := os.Getenv("PARITY_ONLY"); only != "" {
		cmd = []string{"env", "PARITY_ONLY=" + only, "node", "/opt/pi/test/parity.mjs"}
	}
	out, _, err := rt.Exec(ctx, inst.ID, cmd, nil)
	if err != nil {
		t.Fatalf("parity.mjs: %v\n%s", err, tail(string(out), 4000))
	}
	var res struct {
		Results []parityResult `json:"results"`
		Checks  []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
			Text string `json:"text"`
		} `json:"checks"`
		Leftovers string `json:"leftovers"`
	}
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("output unparsable: %v\n%s", err, tail(string(out), 4000))
	}
	bad := 0
	for _, r := range res.Results {
		if !r.Equal {
			bad++
			t.Errorf("differs: %s\n  pi:     %s\n  bridge: %s\n  file equal: %v", r.Name, clip(string(r.Builtin)), clip(string(r.Bridge)), r.FullEqual)
		}
	}
	for _, c := range res.Checks {
		if !c.OK {
			t.Errorf("sandbox boundary: %s → %q", c.Name, c.Text)
		}
	}
	// Every operation of the bridge is logged, with the IDs of this run.
	n := 0
	for _, e := range b.executions() {
		if !strings.HasPrefix(e.ToolCallID, "call_parity_") {
			t.Errorf("foreign ID in the log: %+v", e)
		}
		n++
	}
	// Unit tests of the guard (checkSubagentCall, checkWorkflowMessage) with node --test.
	if out, _, err := rt.Exec(ctx, inst.ID, []string{"node", "--test", "/opt/pi/test/guard.test.mjs"}, nil); err != nil {
		t.Errorf("guard: %v\n%s", err, tail(string(out), 3000))
	} else {
		t.Logf("guard: %s", strings.ReplaceAll(strings.TrimSpace(tail(string(out), 220)), "\n", " | "))
	}
	t.Logf("%d cases, %d differing, %d operations logged, files with full output in /tmp: %s", len(res.Results), bad, n, res.Leftovers)
	if len(res.Results) < 40 && os.Getenv("PARITY_ONLY") == "" {
		t.Fatalf("only %d cases ran", len(res.Results))
	}
}

func clip(s string) string {
	if len(s) > 1500 {
		return s[:700] + " … " + s[len(s)-700:]
	}
	return s
}

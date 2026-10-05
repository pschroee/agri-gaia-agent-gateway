package worker

// Gleichlauf der Umleitung (exec-bridge.ts) mit pis eingebauten Werkzeugen (Code-Review:
// Testlücke „Gleichlauf der Bridge-Werkzeuge gegen pi“, M2, L1, L3, L4, H1). Ein Container aus
// dem Test-Abbild agw-parity ist zugleich Ausführungs-Sandbox und Laufort von pis Werkzeugen:
// pi arbeitet direkt auf seinem Dateisystem, die Bridge über den Socket dieses Tests, dessen
// Werkzeug-Endpunkte (sock.NewPiHandler) mit agw-exec serve in demselben Container ausführen.
// Das Skript images/agw-basis/test/parity.mjs vergleicht je Fall Text, Details und bei langer
// Ausgabe von bash den Inhalt der Datei mit der ganzen Ausgabe.

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
		t.Skip("läuft nur im Go-Container mit Docker-Socket (./dev.sh test)")
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
		t.Fatalf("Überwacher: %v %s", err, f.Error)
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
		t.Fatalf("Ausgabe unlesbar: %v\n%s", err, tail(string(out), 4000))
	}
	bad := 0
	for _, r := range res.Results {
		if !r.Equal {
			bad++
			t.Errorf("abweichend: %s\n  pi:     %s\n  Bridge: %s\n  Datei gleich: %v", r.Name, clip(string(r.Builtin)), clip(string(r.Bridge)), r.FullEqual)
		}
	}
	for _, c := range res.Checks {
		if !c.OK {
			t.Errorf("Grenze der Sandbox: %s → %q", c.Name, c.Text)
		}
	}
	// Jede Operation der Bridge ist protokolliert, mit den IDs dieses Laufs.
	n := 0
	for _, e := range b.executions() {
		if !strings.HasPrefix(e.ToolCallID, "call_parity_") {
			t.Errorf("fremde ID im Protokoll: %+v", e)
		}
		n++
	}
	// Unit-Tests des Wächters (checkSubagentCall, checkWorkflowMessage) mit node --test.
	if out, _, err := rt.Exec(ctx, inst.ID, []string{"node", "--test", "/opt/pi/test/guard.test.mjs"}, nil); err != nil {
		t.Errorf("Wächter: %v\n%s", err, tail(string(out), 3000))
	} else {
		t.Logf("Wächter: %s", strings.ReplaceAll(strings.TrimSpace(tail(string(out), 220)), "\n", " | "))
	}
	t.Logf("%d Fälle, %d abweichend, %d Operationen protokolliert, Dateien mit ganzer Ausgabe in /tmp: %s", len(res.Results), bad, n, res.Leftovers)
	if len(res.Results) < 40 && os.Getenv("PARITY_ONLY") == "" {
		t.Fatalf("nur %d Fälle gelaufen", len(res.Results))
	}
}

func clip(s string) string {
	if len(s) > 1500 {
		return s[:700] + " … " + s[len(s)-700:]
	}
	return s
}

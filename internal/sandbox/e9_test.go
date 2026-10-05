package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/execbox"
	"agw/internal/execproto"
	"agw/internal/rpc"

	"github.com/moby/moby/client"
)

// E9 gegen Docker: der Container von pi (ohne Shell) und der Überwacher
// agw-exec serve in der gehärteten Ausführungs-Sandbox. Läuft nur mit
// AGW_DOCKER_TESTS=1. Label agwpoc.managed=test, damit ein Neustart des
// Orchestrators (Hot Reload) die Container nicht abräumt.

func dockerTest(t *testing.T) (*Runtime, context.Context, string) {
	t.Helper()
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("AGW_DOCKER_TESTS=1 setzen, um gegen Docker zu testen")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	net := "agwpoc_test_e9_" + suffix
	rt, err := New("agwpoc_test_e9_egress_" + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateTestNetwork(ctx, rt.Client(), net, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rt.Client().NetworkRemove(context.Background(), net, client.NetworkRemoveOptions{}) })
	return rt, ctx, net
}

func testLabels() map[string]string {
	return map[string]string{LabelManaged: "test", LabelSlot: "test"}
}

func TestPiContainerWithoutShell(t *testing.T) {
	rt, ctx, net := dockerTest(t)
	image := os.Getenv("AGW_PI_IMAGE")
	if image == "" {
		image = "agwpoc/agw-pi:dev"
	}
	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-pi-" + net[len(net)-6:], Image: image, Labels: testLabels(),
		Args:        []string{"--provider", "deepseek", "--model", "deepseek-flash", "--tools", "read"},
		InternalNet: net, MemoryMB: 512, CPUs: 1, Pids: 128, Tmpfs: PiTmpfs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)
	c := rpc.New(inst.Stdin, inst.Stdout)
	if _, err := c.Call(ctx, map[string]any{"type": "get_state"}); err != nil {
		t.Fatalf("pi ohne Shell antwortet nicht: %v (stderr: %s)", err, inst.Stderr.String())
	}
	// P3: pi ist PID 1, es gibt keine Shell und kein Python.
	out, _, err := rt.Exec(ctx, inst.ID, []string{"readlink", "/proc/1/exe"}, nil)
	title, _, _ := rt.Exec(ctx, inst.ID, []string{"cat", "/proc/1/comm"}, nil)
	if err != nil || strings.TrimSpace(string(out)) != "/usr/local/bin/node" || strings.TrimSpace(string(title)) != "pi" {
		t.Fatalf("PID 1: %q %q %v", out, title, err)
	}
	for _, c := range [][]string{{"sh", "-c", "true"}, {"/bin/bash", "-c", "true"}, {"python3", "-c", "1"}, {"dash", "-c", "true"}} {
		if _, _, err := rt.Exec(ctx, inst.ID, c, nil); err == nil {
			t.Fatalf("%v läuft im Container von pi", c)
		}
	}
	// agw-exec erledigt, was der Orchestrator braucht; /workspace ist leer und schreibgeschützt.
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"agw-exec", "put", "/agent/sessions/x/probe.jsonl"}, strings.NewReader("{}\n")); err != nil {
		t.Fatal(err)
	}
	if out, _, err := rt.Exec(ctx, inst.ID, []string{"cat", "/agent/sessions/x/probe.jsonl"}, nil); err != nil || string(out) != "{}\n" {
		t.Fatalf("put/cat: %q %v", out, err)
	}
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"agw-exec", "put", "/workspace/x"}, strings.NewReader("x")); err == nil {
		t.Fatal("/workspace im Container von pi beschreibbar")
	}
	out, _, _ = rt.Exec(ctx, inst.ID, []string{"agw-exec", "poll-subagents"}, strings.NewReader(`{"offsets":{}}`))
	if !strings.Contains(string(out), `"files":[]`) {
		t.Fatalf("poll-subagents: %q", out)
	}
	if out, _, _ := rt.Exec(ctx, inst.ID, []string{"id", "-u"}, nil); strings.TrimSpace(string(out)) != "10001" {
		t.Fatalf("läuft als %q", out)
	}
	if out, _, err := rt.Exec(ctx, inst.ID, []string{"pi", "--version"}, nil); err != nil || !strings.Contains(string(out), ".") {
		t.Fatalf("pi --version ohne Shell: %q %v", out, err)
	}
	_ = inst.Stdin.Close()
	select {
	case <-inst.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("pi endet nicht nach Schließen von stdin")
	}
}

func TestExecSandboxServe(t *testing.T) {
	rt, ctx, net := dockerTest(t)
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-exec-" + net[len(net)-6:], Image: image, Labels: testLabels(), NoAttach: true,
		InternalNet: net, MemoryMB: 512, CPUs: 1, Pids: 128, Tmpfs: ExecTmpfs, CapAdd: ExecCaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)
	box := execbox.New(func(dctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
		in, out, closeFn, _, err := rt.ExecStream(context.WithoutCancel(dctx), inst.ID, []string{"/usr/local/bin/agw-exec", "serve"}, "0:0")
		return in, out, closeFn, err
	})
	defer box.Close()
	bash := func(ctx context.Context, cmd string) (string, execproto.Frame, error) {
		var b strings.Builder
		f, err := box.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: cmd, Cwd: "/workspace", Env: map[string]string{"PI_SESSION_ID": "s1"}}, func(d []byte) { b.Write(d) })
		return b.String(), f, err
	}
	// Operationen laufen als Agent-Nutzer, mit dessen Home und der Umgebung der Sandbox.
	out, f, err := bash(ctx, "id -u; id -G; echo $HOME; echo $PI_SESSION_ID; echo $MPLBACKEND; cat /proc/self/status | grep ^CapEff")
	if err != nil || f.Exit == nil || *f.Exit != 0 {
		t.Fatalf("bash: %q %+v %v", out, f, err)
	}
	for _, want := range []string{"10001\n10001\n/home/agent\ns1\nAgg\n", "CapEff:\t0000000000000000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q fehlt in %q", want, out)
		}
	}
	if f, err := box.Run(ctx, execproto.Request{Op: execproto.OpWrite, Path: "/workspace/a.txt", Data: []byte("x")}, nil); err != nil || f.Error != "" {
		t.Fatalf("write: %+v %v", f, err)
	}
	if out, _, _ := rt.Exec(ctx, inst.ID, []string{"stat", "-c", "%u", "/workspace/a.txt"}, nil); strings.TrimSpace(string(out)) != "10001" {
		t.Fatalf("Besitzer: %q", out)
	}
	f, _ = box.Run(ctx, execproto.Request{Op: execproto.OpRead, Path: "/workspace/a.txt"}, nil)
	var rr execproto.ReadResult
	_ = json.Unmarshal(f.Result, &rr)
	if string(rr.Data) != "x" {
		t.Fatalf("read: %+v", f)
	}
	// Der Agent erreicht den Überwacher nicht: anderer Nutzer, kein Signal, kein /proc/<pid>/fd.
	out, _, _ = bash(ctx, `p=$(pgrep -f 'agw-exec serve' | head -1); echo pid=$p; kill -9 $p 2>&1 && echo GETOETET; ls /proc/$p/fd 2>&1 | head -1; echo x > /proc/$p/fd/1 2>&1 && echo GESCHRIEBEN; true`)
	if strings.Contains(out, "GETOETET") || strings.Contains(out, "GESCHRIEBEN") || !strings.Contains(out, "ermission denied") {
		t.Fatalf("Überwacher angreifbar: %q", out)
	}
	if f, err := box.Run(ctx, execproto.Request{Op: execproto.OpStat, Path: "/workspace"}, nil); err != nil || f.Error != "" {
		t.Fatalf("Überwacher nach dem Angriff: %+v %v", f, err)
	}
	// L7/N1: Auch der Kindprozess einer Operation (agw-exec op, uid des Agenten) ist nicht über
	// /proc/<pid>/fd/1 beschreibbar (PR_SET_DUMPABLE=0), weder der eines anderen Aufrufs noch
	// der eigene. Ein paralleler Befehl hält dazu eine Operation offen.
	pctx, pcancel := context.WithCancel(ctx)
	go func() { _, _, _ = bash(pctx, "sleep 30") }()
	time.Sleep(300 * time.Millisecond)
	out, _, _ = bash(ctx, `n=0; for d in /proc/[0-9]*; do p=${d#/proc/}; [ "$(tr '\0' ' ' < $d/cmdline 2>/dev/null)" = "/usr/local/bin/agw-exec op " ] || continue; n=$((n+1)); grep -q '^Uid:.*10001' $d/status && echo UID-OK; echo x > $d/fd/1 2>/dev/null && echo GESCHRIEBEN-$p; ls $d/fd >/dev/null 2>&1 && echo LESBAR-$p; done; echo ops=$n`)
	// Bekannte Grenze (dokumentiert, N1): Die Ausgabe eines parallel laufenden Befehls desselben
	// Chats (bash ist wieder „dumpable“) lässt sich über dessen /proc/<pid>/fd/1 ergänzen.
	side, _, _ := bash(ctx, `for d in /proc/[0-9]*; do [ "$(tr '\0' ' ' < $d/cmdline 2>/dev/null)" = "sleep 30 " ] || continue; echo x > $d/fd/1 2>/dev/null && echo PARALLEL-BESCHREIBBAR; done; true`)
	t.Logf("N1, paralleler Befehl: %q", strings.TrimSpace(side))
	pcancel()
	if strings.Contains(out, "GESCHRIEBEN") || strings.Contains(out, "LESBAR") || !strings.Contains(out, "UID-OK") || strings.Contains(out, "ops=0") {
		t.Fatalf("Ausgabe einer Operation angreifbar: %q", out)
	}
	// PID 1 (agw-exec idle) lässt sich vom Agenten nicht beenden, mit keinem Signal (H2: vorher
	// beendeten QUIT, ABRT, TRAP, SYS, ILL, SEGV, BUS, FPE und STKFLT die Sandbox).
	for _, sig := range []string{"TERM", "INT", "HUP", "QUIT", "ABRT", "TRAP", "SYS", "ILL", "SEGV", "BUS", "FPE", "STKFLT",
		"USR1", "USR2", "PIPE", "ALRM", "VTALRM", "PROF", "XCPU", "XFSZ", "PWR", "TSTP", "TTIN", "TTOU", "STOP", "KILL"} {
		out, _, _ := bash(ctx, "kill -"+sig+" 1 2>&1; sleep 0.1; echo lebt")
		if !strings.Contains(out, "lebt") {
			t.Fatalf("SIG%s an PID 1: %q", sig, out)
		}
		ins, err := rt.Client().ContainerInspect(ctx, inst.ID, client.ContainerInspectOptions{})
		if err != nil || ins.Container.State == nil || !ins.Container.State.Running {
			t.Fatalf("Sandbox nach SIG%s an PID 1 beendet: %v", sig, err)
		}
		if f, err := box.Run(ctx, execproto.Request{Op: execproto.OpStat, Path: "/workspace"}, nil); err != nil || f.Error != "" {
			t.Fatalf("Sandbox nach SIG%s an PID 1: %+v %v", sig, f, err)
		}
	}
	// Zombies werden weiter abgeräumt.
	_, _, _ = bash(ctx, "(sleep 0.1 &) ; sleep 0.5")
	if out, _, _ := rt.Exec(ctx, inst.ID, []string{"ps", "-eo", "stat,args"}, nil); strings.Contains(string(out), "Z ") {
		t.Fatalf("Zombie bleibt:\n%s", out)
	}
	// P5: Abbruch beendet den Befehl in der Sandbox, auch einen Hintergrundprozess der Gruppe.
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	start := time.Now()
	_, f, err = bash(cctx, "sleep 297 & sleep 298")
	cancel()
	if err == nil || f.Code != "aborted" || time.Since(start) > 10*time.Second {
		t.Fatalf("Abbruch: %+v %v nach %v", f, err, time.Since(start))
	}
	time.Sleep(500 * time.Millisecond)
	if out, _, _ := rt.Exec(ctx, inst.ID, []string{"ps", "-eo", "args"}, nil); strings.Contains(string(out), "sleep 29") {
		t.Fatalf("Prozess läuft nach dem Abbruch weiter:\n%s", out)
	}
	// Zeitgrenze
	f, _ = box.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: "sleep 30", Cwd: "/workspace", Timeout: 0.5}, nil)
	if f.Code != "timeout" {
		t.Fatalf("Zeitgrenze: %+v", f)
	}
	// P1: langlebiger Überwacher gegen je ein docker exec (Zahlen im Testprotokoll mit -v).
	measure := func(n int, f func()) time.Duration {
		start := time.Now()
		for i := 0; i < n; i++ {
			f()
		}
		return time.Since(start) / time.Duration(n)
	}
	stat := measure(50, func() { _, _ = box.Run(ctx, execproto.Request{Op: execproto.OpStat, Path: "/workspace"}, nil) })
	trueBash := measure(20, func() {
		_, _ = box.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: "true", Cwd: "/workspace"}, nil)
	})
	dockerExec := measure(10, func() { _, _, _ = rt.Exec(ctx, inst.ID, []string{"true"}, nil) })
	t.Logf("P1: stat über den Überwacher %v, bash 'true' %v, docker exec 'true' %v (Mittel)", stat, trueBash, dockerExec)

	// grep und glob mit ripgrep in der Sandbox
	_, _, _ = bash(ctx, "mkdir -p /workspace/src && printf 'eins\\nzwei Nadel\\n' > /workspace/src/a.py")
	f, _ = box.Run(ctx, execproto.Request{Op: execproto.OpGrep, Path: "/workspace", Grep: &execproto.GrepArgs{Pattern: "Nadel"}}, nil)
	if !strings.Contains(string(f.Result), `"path":"src/a.py","line":2`) {
		t.Fatalf("grep: %s %s", f.Result, f.Error)
	}
	f, _ = box.Run(ctx, execproto.Request{Op: execproto.OpGlob, Path: "/workspace", Glob: &execproto.GlobArgs{Pattern: "*.py"}}, nil)
	if !strings.Contains(string(f.Result), "/workspace/src/a.py") {
		t.Fatalf("glob: %s %s", f.Result, f.Error)
	}
	// H2: Das Ende der Sandbox bemerkt der Orchestrator (Done), auch ohne angehängte Ströme.
	select {
	case <-inst.Done():
		t.Fatal("Done vor dem Ende der Sandbox")
	default:
	}
	if _, err := rt.Client().ContainerKill(ctx, inst.ID, client.ContainerKillOptions{Signal: "KILL"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("Ende der Ausführungs-Sandbox nicht bemerkt")
	}
}

// N3: Eine Fork-Bombe erschöpft das PidsLimit, das Prozesse des Agenten und die Kindprozesse des
// Überwachers teilen. Danach (Zeitgrenze) und auch dann, wenn die Bombe ihrer Prozessgruppe
// entkommt (setsid), laufen Operationen wieder.
func TestExecSandboxForkBomb(t *testing.T) {
	rt, ctx, net := dockerTest(t)
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-bomb-" + net[len(net)-6:], Image: image, Labels: testLabels(), NoAttach: true,
		InternalNet: net, MemoryMB: 512, CPUs: 1, Pids: 128, Tmpfs: ExecTmpfs, CapAdd: ExecCaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)
	box := execbox.New(func(dctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
		in, out, closeFn, _, err := rt.ExecStream(context.WithoutCancel(dctx), inst.ID, []string{"/usr/local/bin/agw-exec", "serve"}, "0:0")
		return in, out, closeFn, err
	})
	defer box.Close()
	bash := func(cmd string, timeout float64) (string, execproto.Frame, error) {
		var b strings.Builder
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		f, err := box.Run(cctx, execproto.Request{Op: execproto.OpBash, Command: cmd, Cwd: "/workspace", Timeout: timeout}, func(d []byte) { b.Write(d) })
		return b.String(), f, err
	}
	works := func(what string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			out, f, err := bash("echo lebt", 5)
			sf, serr := box.Run(ctx, execproto.Request{Op: execproto.OpStat, Path: "/workspace"}, nil)
			if err == nil && f.Exit != nil && *f.Exit == 0 && strings.Contains(out, "lebt") && serr == nil && sf.Error == "" {
				return
			}
			last = fmt.Sprintf("%q %+v %v / %+v %v", out, f, err, sf, serr)
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("%s: Werkzeuge gehen nicht wieder: %s", what, last)
	}
	// 1. Bombe in der Prozessgruppe des Befehls (hält das Limit mit schlafenden Prozessen voll),
	// beendet durch die Zeitgrenze
	_, f, _ := bash("while :; do sleep 1000 & done 2>/dev/null", 3)
	t.Logf("Bombe mit Zeitgrenze: %+v", f)
	works("nach der Zeitgrenze")
	// 2. Bombe, die der Prozessgruppe entkommt und weiterläuft
	out, f, err := bash("setsid bash -c 'while :; do sleep 1000 & done' >/dev/null 2>&1 < /dev/null & echo gestartet", 5)
	t.Logf("entkommene Bombe: %q %+v %v", out, f, err)
	time.Sleep(3 * time.Second)
	works("bei laufender entkommener Bombe")
}

package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/execbox"
	"agw/internal/execproto"
)

// Hintergrundaufgaben gegen Docker: Operation bg des Überwachers in der gehärteten
// Ausführungs-Sandbox. Start, Ausgabedatei, Ende, Grenze, Abbruch samt Prozessgruppe, und der
// Überwacher räumt die Gruppe ab, wenn der Agent den Helfer seiner Aufgabe beendet. Label
// agwpoc.managed=test (testLabels), damit ein Neustart des Orchestrators nichts abräumt.
func TestExecSandboxBackground(t *testing.T) {
	rt, ctx, net := dockerTest(t)
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-bg-" + net[len(net)-6:], Image: image, Labels: testLabels(), NoAttach: true,
		InternalNet: net, MemoryMB: 512, CPUs: 1, Pids: 128, Tmpfs: ExecTmpfs, CapAdd: ExecCaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)
	box := execbox.New(func(dctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
		in, out, closeFn, _, err := rt.ExecStream(context.WithoutCancel(dctx), inst.ID, []string{"/usr/local/bin/agw-exec", "serve", "-bg-max", "2"}, "0:0")
		return in, out, closeFn, err
	})
	defer box.Close()
	bash := func(cmd string) string {
		var b strings.Builder
		_, _ = box.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: cmd, Cwd: "/workspace"}, func(d []byte) { b.Write(d) })
		return b.String()
	}
	type run struct {
		pgid  int
		out   strings.Builder
		frame execproto.Frame
		err   error
		done  chan struct{}
		start chan struct{}
	}
	var mu sync.Mutex
	startBg := func(c context.Context, seq int, cmd string) *run {
		r := &run{done: make(chan struct{}), start: make(chan struct{})}
		go func() {
			defer close(r.done)
			r.frame, r.err = box.RunBackground(c, execproto.Request{Op: execproto.OpBg, Command: cmd, Cwd: "/workspace", Spill: execproto.BgLogPath(seq)},
				func(pg int) { r.pgid = pg; close(r.start) }, func(d []byte) { mu.Lock(); r.out.Write(d); mu.Unlock() })
		}()
		return r
	}
	waitStart := func(r *run) {
		t.Helper()
		select {
		case <-r.start:
		case <-r.done:
			t.Fatalf("nicht gestartet: %+v %v", r.frame, r.err)
		case <-time.After(10 * time.Second):
			t.Fatal("Start dauert zu lang")
		}
	}
	waitDone := func(r *run) {
		t.Helper()
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
			t.Fatal("Ende kommt nicht")
		}
	}

	// Review 3, N1: Der Agent legt vorab eine FIFO an die Adresse einer künftigen Ausgabedatei (und
	// einer Ausgabedatei von bash). Das Verzeichnis gehört dem Überwacher (root), dort gelingt das
	// nicht; an der Adresse von bash hält die FIFO den Helfer nicht fest.
	fifo := bash("mkdir -p /tmp/agw-bg; mkfifo /tmp/agw-bg/bg-6.log 2>&1 && echo ANGELEGT; mkfifo " + execproto.SpillPath("call_fifo") + " && echo BASH-FIFO")
	t.Logf("FIFO-Versuch: %q", fifo)
	if strings.Contains(fifo, "ANGELEGT") || !strings.Contains(fifo, "BASH-FIFO") {
		t.Fatalf("FIFO im Verzeichnis der Hintergrundaufgaben: %q", fifo)
	}
	fctx, fcancel := context.WithTimeout(ctx, 10*time.Second)
	var fout strings.Builder
	ff, ferr := box.Run(fctx, execproto.Request{Op: execproto.OpBash, Command: "seq 1 20000", Cwd: "/workspace", Spill: execproto.SpillPath("call_fifo")}, func(d []byte) { fout.Write(d) })
	fcancel()
	if ferr != nil || ff.Exit == nil || *ff.Exit != 0 || ff.SpillError == "" || !strings.HasSuffix(fout.String(), "20000\n") {
		t.Fatalf("bash mit FIFO an der Ausgabedatei: %+v %v", ff, ferr)
	}

	// Ende mit Exit-Code; Ausgabe gestreamt und in /tmp/agw-bg/bg-1.log. Verzeichnis und Datei legt
	// der Überwacher an (root); der Agent liest sie, kann sie aber nicht verändern.
	r1 := startBg(ctx, 1, "echo start; sleep 1; echo fertig-bg; exit 7")
	waitStart(r1)
	waitDone(r1)
	if r1.err != nil || r1.frame.Exit == nil || *r1.frame.Exit != 7 || r1.frame.FullOutputPath != "/tmp/agw-bg/bg-1.log" {
		t.Fatalf("Ende: %+v %v", r1.frame, r1.err)
	}
	if r1.out.String() != "start\nfertig-bg\n" {
		t.Fatalf("Ausgabe: %q", r1.out.String())
	}
	if out := bash("stat -c '%u %a' /tmp/agw-bg /tmp/agw-bg/bg-1.log; cat /tmp/agw-bg/bg-1.log; (echo x >> /tmp/agw-bg/bg-1.log) 2>/dev/null || echo SCHREIBSCHUTZ; touch /tmp/agw-bg/neu 2>/dev/null || echo KEIN-ANLEGEN"); out != "0 755\n0 644\nstart\nfertig-bg\nSCHREIBSCHUTZ\nKEIN-ANLEGEN\n" {
		t.Fatalf("Datei: %q", out)
	}

	// Grenze 2: zwei laufen, die dritte wird abgewiesen; bash geht weiter.
	c2, cancel2 := context.WithCancel(ctx)
	r2 := startBg(c2, 2, "sleep 300 & sleep 301; echo nie")
	r3 := startBg(ctx, 3, "sleep 302")
	waitStart(r2)
	waitStart(r3)
	r4 := startBg(ctx, 4, "true")
	waitDone(r4)
	if r4.frame.Code != "ELIMIT" || !strings.Contains(r4.frame.Error, "limit 2") {
		t.Fatalf("Grenze: %+v", r4.frame)
	}
	if out := bash("echo frei"); out != "frei\n" {
		t.Fatalf("bash neben zwei Hintergrundaufgaben: %q", out)
	}

	// Abbruch (bg_stop): Die ganze Prozessgruppe ist danach weg, auch der Hintergrundprozess.
	stopAt := time.Now()
	cancel2()
	waitDone(r2)
	if r2.frame.Code != "aborted" {
		t.Fatalf("Abbruch: %+v %v", r2.frame, r2.err)
	}
	t.Logf("Abbruch nach %d ms", time.Since(stopAt).Milliseconds())
	if out := bash("pgrep -f 'sleep 30[01]' || echo keine"); out != "keine\n" {
		t.Fatalf("Prozesse nach dem Abbruch: %q", out)
	}

	// Der Agent beendet den Helfer seiner Aufgabe (gleicher Nutzer, SIGKILL): Der Überwacher
	// (root mit CAP_KILL) räumt die Gruppe ab, die Operation endet mit Fehler.
	out := bash(`for d in /proc/[0-9]*; do [ "$(tr '\0' ' ' < $d/cmdline 2>/dev/null)" = "sleep 302 " ] || continue; p=${d#/proc/}; while [ "$p" -gt 1 ] && [ "$(tr '\0' ' ' < /proc/$p/cmdline)" != "/usr/local/bin/agw-exec op " ]; do p=$(awk '/^PPid/{print $2}' /proc/$p/status); done; echo "helfer=$(tr '\0' ' ' < /proc/$p/cmdline)"; kill -9 $p && echo GETOETET; done`)
	if !strings.Contains(out, "helfer=/usr/local/bin/agw-exec op") || !strings.Contains(out, "GETOETET") {
		t.Fatalf("Helfer nicht gefunden: %q", out)
	}
	waitDone(r3)
	if r3.frame.Code != "EIO" {
		t.Fatalf("Ende ohne Helfer: %+v %v", r3.frame, r3.err)
	}
	time.Sleep(200 * time.Millisecond)
	if out := bash("pgrep -f 'sleep 30[2]' || echo keine"); out != "keine\n" {
		t.Fatalf("Gruppe lebt ohne Helfer weiter: %q", out)
	}

	// Danach ist wieder Platz; die Datei ist nicht über 256 MiB gewachsen (Stichprobe: groß).
	r5 := startBg(ctx, 5, "head -c 3000000 /dev/zero | tr '\\0' y; echo; echo ende")
	waitStart(r5)
	waitDone(r5)
	if r5.frame.Exit == nil || *r5.frame.Exit != 0 {
		t.Fatalf("nach der Grenze: %+v %v", r5.frame, r5.err)
	}
	if out := bash("stat -c %s /tmp/agw-bg/bg-5.log; tail -1 /tmp/agw-bg/bg-5.log"); out != "3000006\nende\n" {
		t.Fatalf("große Ausgabe: %q", out)
	}
	// Die Aufgabe, deren Ausgabedatei der Agent vorab als FIFO anlegen wollte, läuft normal.
	rf := startBg(ctx, 6, "echo sechs")
	waitStart(rf)
	waitDone(rf)
	if rf.frame.Exit == nil || *rf.frame.Exit != 0 || rf.frame.SpillError != "" {
		t.Fatalf("bg-6: %+v %v", rf.frame, rf.err)
	}
	// Die Notbremse (N3) gilt weiter: Nach einer Fork-Bombe startet wieder eine Hintergrundaufgabe.
	bctx, bcancel := context.WithTimeout(ctx, 20*time.Second)
	_, _ = box.Run(bctx, execproto.Request{Op: execproto.OpBash, Command: "b(){ b|b & }; b", Cwd: "/workspace", Timeout: 3}, nil)
	bcancel()
	// Wie in TestExecSandboxForkBomb: kurz nach der Bombe kann ein Start noch an EAGAIN scheitern;
	// innerhalb von 20 s muss es wieder gehen.
	var r6 *run
	for i, deadline := 7, time.Now().Add(20*time.Second); ; i++ {
		r6 = startBg(ctx, i, "echo nach-der-bombe")
		waitDone(r6)
		if r6.frame.Exit != nil && *r6.frame.Exit == 0 && strings.Contains(r6.out.String(), "nach-der-bombe") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nach der Fork-Bombe: %+v %v %q", r6.frame, r6.err, r6.out.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := json.Marshal(r6.frame)
	t.Logf("letzte Aufgabe: %s", b)
}

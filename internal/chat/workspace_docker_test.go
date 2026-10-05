package chat

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/sandbox"

	"github.com/moby/moby/client"
)

type rtExec struct {
	rt *sandbox.Runtime
	id string
}

func (r rtExec) Exec(ctx context.Context, cmd []string, stdin io.Reader) ([]byte, error) {
	out, _, err := r.rt.Exec(ctx, r.id, cmd, stdin)
	return out, err
}

// Packen und Auspacken in der echten, gehärteten Sandbox (read-only root, tmpfs,
// uid 10001): Dateien, Ordner, Rechte und Symlinks kommen an; inputs/, Paket- und
// Cache-Ordner nicht; /tmp und Home gehören nicht zum Arbeitsbereich. Läuft nur
// mit AGW_DOCKER_TESTS=1.
func TestWorkspaceRoundTripInSandbox(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("AGW_DOCKER_TESTS=1 setzen, um gegen Docker zu testen")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	suffix := time.Now().Format("150405.000")
	suffix = strings.ReplaceAll(suffix, ".", "")
	netName := "agwpoc_test_ws_" + suffix
	rt, err := sandbox.New("agwpoc_test_ws_egress_" + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.CreateTestNetwork(ctx, rt.Client(), netName, true); err != nil {
		t.Fatal(err)
	}
	// t.Cleanup statt defer: Aufräumfunktionen laufen nach den defers und in umgekehrter Reihenfolge,
	// also erst die Container (unten registriert), dann das Netz. Mit defer blieb das Netz stehen, weil
	// noch Container daran hingen; ein liegengebliebenes Netz in 172.25.0.0/16 verdeckte am 05.10.2026
	// die VPN-Adresse der Agri-Gaia-API.
	t.Cleanup(func() { _, _ = rt.Client().NetworkRemove(context.Background(), netName, client.NetworkRemoveOptions{}) })
	start := func(n string) rtExec {
		inst, err := rt.Start(ctx, sandbox.Spec{
			Name: "agwpoc-test-ws-" + n + "-" + suffix, Image: image, NoAttach: true, Tmpfs: sandbox.ExecTmpfs,
			Labels:      map[string]string{sandbox.LabelSlot: "test-ws", sandbox.LabelManaged: "test"},
			InternalNet: netName, MemoryMB: 1024, CPUs: 1, Pids: 128,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rt.Remove(context.Background(), inst.ID) })
		return rtExec{rt: rt, id: inst.ID}
	}
	src, dst := start("a"), start("b")

	setup := `set -e
cd /workspace
mkdir -p sub/tief inputs node_modules/paket .venv/lib pkg/__pycache__ .cache
printf 'hallo\n' > notiz.txt
printf 'print(1)\n' > sub/tief/skript.py && chmod 755 sub/tief/skript.py
python3 -c "import matplotlib.pyplot as plt; plt.plot([1,2]); plt.savefig('plot.png')"
printf 'x' > inputs/daten.csv
printf 'x' > node_modules/paket/index.js
printf 'x' > .venv/lib/a.py
printf 'x' > pkg/__pycache__/a.pyc
printf 'x' > .cache/c
printf 'x' > pkg/modul.py
ln -s /etc/passwd aussen
ln -s notiz.txt innen
mkfifo rohr
echo tmp > /tmp/weg.txt
echo home > /home/agent/weg.txt`
	if _, err := src.Exec(ctx, []string{"bash", "-c", setup}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshotWorkspace(ctx, src, 10<<20, "")
	if err != nil {
		t.Fatal(err)
	}
	// notiz.txt, skript.py, plot.png, pkg/modul.py; Symlinks und FIFO zählen nicht als Dateien.
	if snap.Status != "DATA" || snap.Files != 4 || len(snap.Archive) == 0 {
		t.Fatalf("Sicherung: %s Dateien=%d Größe=%d Archiv=%d", snap.Status, snap.Files, snap.Size, len(snap.Archive))
	}
	// Unverändert: gleicher Fingerabdruck, nichts gepackt.
	again, err := snapshotWorkspace(ctx, src, 10<<20, snap.Fingerprint)
	if err != nil || again.Status != "SAME" {
		t.Fatalf("zweiter Lauf: %+v %v", again.Status, err)
	}
	// Grenze: nichts gepackt.
	if s, err := snapshotWorkspace(ctx, src, 10, ""); err != nil || s.Status != "SKIP" || s.Archive != nil {
		t.Fatalf("Grenze: %s %v", s.Status, err)
	}

	kept, dropped, err := restoreWorkspaceArchive(ctx, dst, snap.Archive, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 { // die FIFO
		t.Errorf("verworfen: %d (übernommen %d)", dropped, kept)
	}
	check := `cd /workspace
cat notiz.txt
test -x sub/tief/skript.py && echo ausfuehrbar
head -c 4 plot.png | tail -c 3; echo
readlink aussen; readlink innen
cat pkg/modul.py; echo
for p in inputs node_modules .venv pkg/__pycache__ .cache rohr /tmp/weg.txt /home/agent/weg.txt; do test -e "$p" && echo "DA: $p"; done
stat -c '%u' notiz.txt`
	out, err := dst.Exec(ctx, []string{"bash", "-c", check}, nil)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "hallo\nausfuehrbar\nPNG\n/etc/passwd\nnotiz.txt\nx\n10001\n"
	if string(out) != want {
		t.Fatalf("nach dem Einspielen:\n%s\nerwartet:\n%s", out, want)
	}
	// In der neuen Sandbox gleicher Stand: Das Ruhen danach packt nicht erneut.
	if s, err := snapshotWorkspace(ctx, dst, 10<<20, snap.Fingerprint); err != nil || s.Status != "SAME" {
		t.Errorf("nach dem Einspielen verändert: %s %v (Dateien %d)", s.Status, err, s.Files)
	}
	// Ein vom Agenten gebautes Archiv schreibt nicht über einen Symlink aus /workspace hinaus.
	evil := makeTarGz(t, []tarEntry{
		{name: "./esc", typ: tar.TypeSymlink, link: "/home/agent"},
		{name: "./esc/boese", typ: tar.TypeReg, body: "x"},
		{name: "../../home/agent/boese2", typ: tar.TypeReg, body: "x"},
	})
	if _, _, err := restoreWorkspaceArchive(ctx, dst, evil, 1<<20); err != nil {
		t.Fatal(err)
	}
	if out, _ := dst.Exec(ctx, []string{"bash", "-c", "ls /home/agent; readlink /workspace/esc"}, nil); strings.Contains(string(out), "boese") || !strings.Contains(string(out), "/home/agent") {
		t.Fatalf("außerhalb geschrieben: %s", out)
	}
}

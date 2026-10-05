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

// Packing and unpacking in the real, hardened sandbox (read-only root, tmpfs,
// uid 10001): files, folders, permissions and symlinks arrive; inputs/, package
// and cache folders do not; /tmp and home are not part of the workspace. Runs
// only with AGW_DOCKER_TESTS=1.
func TestWorkspaceRoundTripInSandbox(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
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
	// t.Cleanup instead of defer: cleanup functions run after the defers and in reverse order, so first
	// the containers (registered below), then the network. With defer the network stayed, because
	// containers were still attached to it; a leftover network in 172.25.0.0/16 hid the VPN address of
	// the Agri-Gaia API on 2026-10-05.
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
mkdir -p sub/deep inputs node_modules/package .venv/lib pkg/__pycache__ .cache
printf 'hello\n' > note.txt
printf 'print(1)\n' > sub/deep/script.py && chmod 755 sub/deep/script.py
python3 -c "import matplotlib.pyplot as plt; plt.plot([1,2]); plt.savefig('plot.png')"
printf 'x' > inputs/data.csv
printf 'x' > node_modules/package/index.js
printf 'x' > .venv/lib/a.py
printf 'x' > pkg/__pycache__/a.pyc
printf 'x' > .cache/c
printf 'x' > pkg/module.py
ln -s /etc/passwd outside
ln -s note.txt inside
mkfifo pipe
echo tmp > /tmp/gone.txt
echo home > /home/agent/gone.txt`
	if _, err := src.Exec(ctx, []string{"bash", "-c", setup}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshotWorkspace(ctx, src, 10<<20, "")
	if err != nil {
		t.Fatal(err)
	}
	// note.txt, script.py, plot.png, pkg/module.py; symlinks and FIFO do not count as files.
	if snap.Status != "DATA" || snap.Files != 4 || len(snap.Archive) == 0 {
		t.Fatalf("backup: %s files=%d size=%d archive=%d", snap.Status, snap.Files, snap.Size, len(snap.Archive))
	}
	// Unchanged: same fingerprint, nothing packed.
	again, err := snapshotWorkspace(ctx, src, 10<<20, snap.Fingerprint)
	if err != nil || again.Status != "SAME" {
		t.Fatalf("second run: %+v %v", again.Status, err)
	}
	// Limit: nothing packed.
	if s, err := snapshotWorkspace(ctx, src, 10, ""); err != nil || s.Status != "SKIP" || s.Archive != nil {
		t.Fatalf("limit: %s %v", s.Status, err)
	}

	kept, dropped, err := restoreWorkspaceArchive(ctx, dst, snap.Archive, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 { // the FIFO
		t.Errorf("dropped: %d (kept %d)", dropped, kept)
	}
	check := `cd /workspace
cat note.txt
test -x sub/deep/script.py && echo executable
head -c 4 plot.png | tail -c 3; echo
readlink outside; readlink inside
cat pkg/module.py; echo
for p in inputs node_modules .venv pkg/__pycache__ .cache pipe /tmp/gone.txt /home/agent/gone.txt; do test -e "$p" && echo "THERE: $p"; done
stat -c '%u' note.txt`
	out, err := dst.Exec(ctx, []string{"bash", "-c", check}, nil)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "hello\nexecutable\nPNG\n/etc/passwd\nnote.txt\nx\n10001\n"
	if string(out) != want {
		t.Fatalf("after restoring:\n%s\nwant:\n%s", out, want)
	}
	// Same state in the new sandbox: idling afterwards does not pack again.
	if s, err := snapshotWorkspace(ctx, dst, 10<<20, snap.Fingerprint); err != nil || s.Status != "SAME" {
		t.Errorf("changed after restoring: %s %v (files %d)", s.Status, err, s.Files)
	}
	// An archive built by the agent does not write out of /workspace via a symlink.
	evil := makeTarGz(t, []tarEntry{
		{name: "./esc", typ: tar.TypeSymlink, link: "/home/agent"},
		{name: "./esc/evil", typ: tar.TypeReg, body: "x"},
		{name: "../../home/agent/evil2", typ: tar.TypeReg, body: "x"},
	})
	if _, _, err := restoreWorkspaceArchive(ctx, dst, evil, 1<<20); err != nil {
		t.Fatal(err)
	}
	if out, _ := dst.Exec(ctx, []string{"bash", "-c", "ls /home/agent; readlink /workspace/esc"}, nil); strings.Contains(string(out), "evil") || !strings.Contains(string(out), "/home/agent") {
		t.Fatalf("written outside: %s", out)
	}
}

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// Integration test against the local Docker daemon and the image of the
// execution sandbox agwpoc/agw-basis:dev. Runs only with AGW_DOCKER_TESTS=1.
func TestSandboxIntegration(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	suffix := time.Now().Format("150405")
	internal, egress := "agwpoc_test_internal_"+suffix, "agwpoc_test_egress_"+suffix
	rt, err := New(egress)
	if err != nil {
		t.Fatal(err)
	}
	cli := rt.Client()
	if err := CreateTestNetwork(ctx, cli, internal, true); err != nil {
		t.Fatal(err)
	}
	defer cli.NetworkRemove(context.Background(), internal, client.NetworkRemoveOptions{})
	if err := CreateTestNetwork(ctx, cli, egress, false); err != nil {
		t.Fatal(err)
	}
	defer cli.NetworkRemove(context.Background(), egress, client.NetworkRemoveOptions{})

	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-" + suffix, Image: image, NoAttach: true,
		Labels:      map[string]string{LabelSlot: "test", LabelManaged: "test"},
		InternalNet: internal, MemoryMB: 512, CPUs: 1, Pids: 128,
		Tmpfs: ExecTmpfs, CapAdd: ExecCaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), inst.ID)

	// 2. exec writes and reads in tmpfs.
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "cat > /workspace/probe.txt"}, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	out, _, err := rt.Exec(ctx, inst.ID, []string{"cat", "/workspace/probe.txt"}, nil)
	if err != nil || string(out) != "hello" {
		t.Fatalf("exec read: %q %v", out, err)
	}

	// 3. Hardening: root read-only, user not root.
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "touch /usr/x"}, nil); err == nil {
		t.Fatal("root file system is writable")
	}
	out, _, _ = rt.Exec(ctx, inst.ID, []string{"id", "-u"}, nil)
	if strings.TrimSpace(string(out)) != "10001" {
		t.Fatalf("runs as %q", out)
	}

	// 3b. Working directory, home and /tmp allow execution: scripts and
	// compiled libraries (e.g. numpy .so in ~/.local) must run.
	for _, dir := range []string{"/workspace", "/home/agent", "/tmp"} {
		out, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "printf '#!/bin/sh\necho runs\n' > " + dir + "/x.sh && chmod +x " + dir + "/x.sh && " + dir + "/x.sh"}, nil)
		if err != nil || strings.TrimSpace(string(out)) != "runs" {
			t.Fatalf("executing in %s: %q %v", dir, out, err)
		}
	}
	// Load preinstalled data packages with compiled parts.
	out, _, err = rt.Exec(ctx, inst.ID, []string{"python3", "-c", "import numpy, pandas, matplotlib, jinja2, plotly; print('ok')"}, nil)
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("data packages: %q %v", out, err)
	}
	// Charts: matplotlib writes a PNG without a display and without internet (skill charts,
	// display images in the chat). MPLBACKEND=Agg comes from the image.
	plot := `import matplotlib.pyplot as plt, numpy as np, pandas as pd, os
assert os.environ.get("MPLBACKEND") == "Agg", os.environ.get("MPLBACKEND")
df = pd.DataFrame({"x": np.arange(5), "y": np.arange(5) ** 2})
fig, ax = plt.subplots(figsize=(4, 3))
df.plot(x="x", y="y", ax=ax)
fig.savefig("/workspace/probe.png", dpi=100, bbox_inches="tight")
plt.close(fig)`
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"python3", "-c", plot}, nil); err != nil {
		t.Fatalf("matplotlib: %v", err)
	}
	out, _, err = rt.Exec(ctx, inst.ID, []string{"head", "-c", "8", "/workspace/probe.png"}, nil)
	if err != nil || string(out) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("no PNG: %q %v", out, err)
	}
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"test", "-s", "/opt/agw/skills/charts/SKILL.md"}, nil); err != nil {
		t.Fatalf("skill charts missing: %v", err)
	}
	// The read script for display images needs realpath and head -c.
	out, _, err = rt.Exec(ctx, inst.ID, []string{"sh", "-c", `p=$(realpath -e -- "$1") && head -c 8 -- "$p"`, "sh", "/workspace/./probe.png"}, nil)
	if err != nil || string(out) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("realpath/head: %q %v", out, err)
	}
	out, _, _ = rt.Exec(ctx, inst.ID, []string{"sh", "-c", "echo $PATH"}, nil)
	if !strings.Contains(string(out), "/home/agent/.local/bin") {
		t.Fatalf("PATH without ~/.local/bin: %q", out)
	}

	// 3c. Typst with the built-in packages, without internet.
	doc := `#import "@preview/cetz:0.5.2"
#import "@preview/fletcher:0.5.8": diagram, node
= Test
#diagram(node((0, 0), [A]))
#cetz.canvas({ cetz.draw.circle((0, 0)) })`
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "cat > /workspace/t.typ && cd /workspace && typst compile t.typ && test -s t.pdf"}, strings.NewReader(doc)); err != nil {
		t.Fatalf("Typst with built-in packages: %v", err)
	}

	// 4. Internet switch: without egress no way out, with egress there is.
	probe := []string{"sh", "-c", "curl -s -o /dev/null -m 8 -w '%{http_code}' https://example.com || echo FAIL"}
	out, _, _ = rt.Exec(ctx, inst.ID, probe, nil)
	if !strings.Contains(string(out), "FAIL") {
		t.Fatalf("internet reachable without egress network: %q", out)
	}
	if err := rt.SetInternet(ctx, inst.ID, true); err != nil {
		t.Fatal(err)
	}
	out, _, _ = rt.Exec(ctx, inst.ID, probe, nil)
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("no internet despite egress network: %q", out)
	}
	// With internet no further Typst package can be loaded (cache read-only).
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", `printf '#import "@preview/tidy:0.4.3"\nx' > /workspace/u.typ && cd /workspace && typst compile u.typ`}, nil); err == nil {
		t.Fatal("non-built-in Typst package could be loaded")
	}
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatal(err)
	}
	out, _, _ = rt.Exec(ctx, inst.ID, probe, nil)
	if !strings.Contains(string(out), "FAIL") {
		t.Fatalf("internet still reachable after disconnecting: %q", out)
	}
	// Disconnecting twice is not an error.
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatalf("disconnecting twice: %v", err)
	}

	// No pi in the execution sandbox (E9).
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "command -v pi"}, nil); err == nil {
		t.Fatal("pi in the execution sandbox")
	}
}

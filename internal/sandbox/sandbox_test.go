package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// Integrationstest gegen den lokalen Docker-Daemon und das Abbild der
// Ausführungs-Sandbox agwpoc/agw-basis:dev. Läuft nur mit AGW_DOCKER_TESTS=1.
func TestSandboxIntegration(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("AGW_DOCKER_TESTS=1 setzen, um gegen Docker zu testen")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	suffix := time.Now().Format("150405")
	internal, egress := "agwpoc_test_intern_"+suffix, "agwpoc_test_egress_"+suffix
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

	// 2. exec schreibt und liest im tmpfs.
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "cat > /workspace/probe.txt"}, strings.NewReader("hallo")); err != nil {
		t.Fatal(err)
	}
	out, _, err := rt.Exec(ctx, inst.ID, []string{"cat", "/workspace/probe.txt"}, nil)
	if err != nil || string(out) != "hallo" {
		t.Fatalf("exec lesen: %q %v", out, err)
	}

	// 3. Härtung: Wurzel schreibgeschützt, Nutzer nicht root.
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "touch /usr/x"}, nil); err == nil {
		t.Fatal("Wurzeldateisystem ist beschreibbar")
	}
	out, _, _ = rt.Exec(ctx, inst.ID, []string{"id", "-u"}, nil)
	if strings.TrimSpace(string(out)) != "10001" {
		t.Fatalf("läuft als %q", out)
	}

	// 3b. Arbeitsverzeichnis, Home und /tmp erlauben Ausführung: Skripte und
	// kompilierte Bibliotheken (etwa numpy-.so in ~/.local) müssen laufen.
	for _, dir := range []string{"/workspace", "/home/agent", "/tmp"} {
		out, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "printf '#!/bin/sh\necho lauf\n' > " + dir + "/x.sh && chmod +x " + dir + "/x.sh && " + dir + "/x.sh"}, nil)
		if err != nil || strings.TrimSpace(string(out)) != "lauf" {
			t.Fatalf("Ausführen in %s: %q %v", dir, out, err)
		}
	}
	// Vorinstallierte Datenpakete mit kompilierten Teilen laden.
	out, _, err = rt.Exec(ctx, inst.ID, []string{"python3", "-c", "import numpy, pandas, matplotlib, jinja2, plotly; print('ok')"}, nil)
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("Datenpakete: %q %v", out, err)
	}
	// Diagramme: matplotlib schreibt ohne Bildschirm und ohne Internet ein PNG (Skill diagramme,
	// Anzeige-Bilder im Chat). MPLBACKEND=Agg kommt aus dem Abbild.
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
		t.Fatalf("kein PNG: %q %v", out, err)
	}
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"test", "-s", "/opt/agw/skills/diagramme/SKILL.md"}, nil); err != nil {
		t.Fatalf("Skill diagramme fehlt: %v", err)
	}
	// Das Leseskript für Anzeige-Bilder braucht realpath und head -c.
	out, _, err = rt.Exec(ctx, inst.ID, []string{"sh", "-c", `p=$(realpath -e -- "$1") && head -c 8 -- "$p"`, "sh", "/workspace/./probe.png"}, nil)
	if err != nil || string(out) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("realpath/head: %q %v", out, err)
	}
	out, _, _ = rt.Exec(ctx, inst.ID, []string{"sh", "-c", "echo $PATH"}, nil)
	if !strings.Contains(string(out), "/home/agent/.local/bin") {
		t.Fatalf("PATH ohne ~/.local/bin: %q", out)
	}

	// 3c. Typst mit den eingebauten Paketen, ohne Internet.
	doc := `#import "@preview/cetz:0.5.2"
#import "@preview/fletcher:0.5.8": diagram, node
= Test
#diagram(node((0, 0), [A]))
#cetz.canvas({ cetz.draw.circle((0, 0)) })`
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "cat > /workspace/t.typ && cd /workspace && typst compile t.typ && test -s t.pdf"}, strings.NewReader(doc)); err != nil {
		t.Fatalf("Typst mit eingebauten Paketen: %v", err)
	}

	// 4. Internet-Schalter: ohne Egress kein Ausgang, mit Egress schon.
	probe := []string{"sh", "-c", "curl -s -o /dev/null -m 8 -w '%{http_code}' https://example.com || echo FAIL"}
	out, _, _ = rt.Exec(ctx, inst.ID, probe, nil)
	if !strings.Contains(string(out), "FAIL") {
		t.Fatalf("Internet erreichbar ohne Egress-Netz: %q", out)
	}
	if err := rt.SetInternet(ctx, inst.ID, true); err != nil {
		t.Fatal(err)
	}
	out, _, _ = rt.Exec(ctx, inst.ID, probe, nil)
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("kein Internet trotz Egress-Netz: %q", out)
	}
	// Mit Internet lässt sich kein weiteres Typst-Paket nachladen (Cache schreibgeschützt).
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", `printf '#import "@preview/tidy:0.4.3"\nx' > /workspace/u.typ && cd /workspace && typst compile u.typ`}, nil); err == nil {
		t.Fatal("nicht eingebautes Typst-Paket ließ sich laden")
	}
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatal(err)
	}
	out, _, _ = rt.Exec(ctx, inst.ID, probe, nil)
	if !strings.Contains(string(out), "FAIL") {
		t.Fatalf("Internet nach dem Trennen noch erreichbar: %q", out)
	}
	// Doppeltes Trennen ist kein Fehler.
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatalf("doppeltes Trennen: %v", err)
	}

	// Kein pi in der Ausführungs-Sandbox (E9).
	if _, _, err := rt.Exec(ctx, inst.ID, []string{"sh", "-c", "command -v pi"}, nil); err == nil {
		t.Fatal("pi in der Ausführungs-Sandbox")
	}
}

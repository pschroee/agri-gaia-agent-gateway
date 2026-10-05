package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// TestPkgCacheIntegration prüft die Paket-Zwischenspeicher mit eigenen Netzen
// und eigenen Verdaccio-/proxpi-Containern (Abbilder wie in compose.yaml):
//
//	(a) ohne Internet sind npm-cache und pip-cache aus der Sandbox nicht erreichbar,
//	(b) mit Internet gehen pip install und npm install über die Zwischenspeicher,
//	(c) ein zweiter Abruf kommt aus dem Zwischenspeicher: Die Zwischenspeicher
//	    verlieren dafür ihr Internet und die Sandbox ihr Egress-Netz,
//	(d) nach dem Abschalten ist wieder nichts erreichbar, und ein Platz-Netz mit
//	    angehängtem Zwischenspeicher lässt sich abbauen.
//
// Läuft nur mit AGW_DOCKER_TESTS=1 und braucht Internet am Host.
func TestPkgCacheIntegration(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("AGW_DOCKER_TESTS=1 setzen, um gegen Docker zu testen")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	npmImage, pipImage := composeImage(t, "verdaccio/verdaccio"), composeImage(t, "epicwink/proxpi")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	bg := context.Background()

	suffix := time.Now().Format("150405")
	slotNet, egress, upstream := "agwpoc_test_pkgslot_"+suffix, "agwpoc_test_pkgegress_"+suffix, "agwpoc_test_pkgup_"+suffix
	rt, err := New(egress)
	if err != nil {
		t.Fatal(err)
	}
	cli := rt.Client()
	for _, n := range []struct {
		name     string
		internal bool
	}{{slotNet, true}, {upstream, false}} {
		if err := CreateTestNetwork(ctx, cli, n.name, n.internal); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.EnsureEgressNetwork(ctx, TestSubnet()); err != nil { // ohne ICC wie im Betrieb
		t.Fatal(err)
	}
	defer func() {
		for _, n := range []string{slotNet, egress, upstream} {
			_, _ = cli.NetworkRemove(bg, n, client.NetworkRemoveOptions{})
		}
	}()

	// Zwischenspeicher wie in compose.yaml, aber mit eigenen Volumes.
	cfg, err := filepath.Abs("../../pkgcache/verdaccio.yaml")
	if err != nil {
		t.Fatal(err)
	}
	npmName, pipName := "agwpoc-test-npmcache-"+suffix, "agwpoc-test-pipcache-"+suffix
	startCache(ctx, t, cli, npmName, npmImage, upstream, nil, []mount.Mount{
		{Type: mount.TypeVolume, Source: npmName, Target: "/verdaccio/storage"},
		{Type: mount.TypeBind, Source: cfg, Target: "/verdaccio/conf/config.yaml", ReadOnly: true},
	})
	startCache(ctx, t, cli, pipName, pipImage, upstream,
		[]string{"PROXPI_CACHE_DIR=/var/cache/proxpi", "PROXPI_DOWNLOAD_TIMEOUT=120", "PROXPI_CONNECT_TIMEOUT=10", "PROXPI_READ_TIMEOUT=60"},
		[]mount.Mount{{Type: mount.TypeVolume, Source: pipName, Target: "/var/cache/proxpi"}})
	rt.SetPkgCaches(npmName, pipName)

	inst, err := rt.Start(ctx, Spec{
		Name: "agwpoc-test-pkg-" + suffix, Image: image, NoAttach: true, Tmpfs: ExecTmpfs,
		Env: rt.PkgCacheEnv(),
		// Nicht "true": Ein Neustart des Orchestrators (Hot Reload) räumte die
		// Test-Sandbox sonst mitten im Test ab (RemoveManaged).
		Labels:      map[string]string{LabelSlot: "test-pkg", LabelManaged: "test"},
		InternalNet: slotNet, MemoryMB: 1024, CPUs: 1, Pids: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	defer func() {
		if !removed {
			_ = rt.Remove(bg, inst.ID)
		}
	}()
	sh := func(script string) (string, error) {
		out, _, err := rt.Exec(ctx, inst.ID, []string{"bash", "-c", script}, nil)
		return string(out), err
	}
	unreachable := func(when string) {
		t.Helper()
		for _, u := range []string{"http://npm-cache:4873/-/ping", "http://pip-cache:5000/"} {
			if out, err := sh("curl -fsS -m 5 -o /dev/null " + u + " && echo erreichbar"); err == nil {
				t.Fatalf("%s: %s erreichbar (%s)", when, u, out)
			}
		}
		start := time.Now()
		// pip download und ein leerer npm-Cache: Beides muss den Index fragen,
		// auch wenn das Paket schon installiert ist.
		if out, err := sh("pip download --no-deps --no-cache-dir -d $(mktemp -d) iniconfig==2.0.0 2>&1"); err == nil {
			t.Fatalf("%s: pip download gelang ohne Internet: %s", when, out)
		}
		t.Logf("%s: pip download scheitert nach %.1f s", when, time.Since(start).Seconds())
		start = time.Now()
		if out, err := sh("rm -rf ~/.npm && cd $(mktemp -d) && npm install --no-audit --no-fund is-number@7.0.0 2>&1"); err == nil {
			t.Fatalf("%s: npm install gelang ohne Internet: %s", when, out)
		}
		t.Logf("%s: npm install scheitert nach %.1f s", when, time.Since(start).Seconds())
	}

	// (a) Ohne Internet: kein Weg zu den Zwischenspeichern.
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatal(err)
	}
	unreachable("(a) ohne Internet")

	// (b) Mit Internet: Installation über die Zwischenspeicher.
	if err := rt.SetInternet(ctx, inst.ID, true); err != nil {
		t.Fatal(err)
	}
	if out, err := sh(`for i in $(seq 1 60); do curl -fsS -m 2 -o /dev/null http://npm-cache:4873/-/ping && curl -fsS -m 2 -o /dev/null http://pip-cache:5000/ && exit 0; sleep 0.5; done; exit 1`); err != nil {
		t.Fatalf("(b) Zwischenspeicher nicht erreichbar: %v %s", err, out)
	}
	start := time.Now()
	out, err := sh("pip install --no-deps --no-cache-dir iniconfig==2.0.0 2>&1")
	if err != nil || !strings.Contains(out, "pip-cache:5000") {
		t.Fatalf("(b) pip install: %v\n%s", err, out)
	}
	t.Logf("(b) pip install über pip-cache: %.1f s", time.Since(start).Seconds())
	start = time.Now()
	if out, err := sh("mkdir -p /workspace/p && cd /workspace/p && npm install --no-audit --no-fund is-number@7.0.0 2>&1 && grep -o 'npm-cache:4873[^\"]*' package-lock.json"); err != nil {
		t.Fatalf("(b) npm install: %v\n%s", err, out)
	}
	t.Logf("(b) npm install über npm-cache: %.1f s", time.Since(start).Seconds())

	// (c) Zweiter Abruf: Zwischenspeicher ohne Upstream, Sandbox ohne Egress.
	// Gelingt die Installation dann noch, kam sie aus dem Zwischenspeicher.
	for _, c := range []string{npmName, pipName} {
		if _, err := cli.NetworkDisconnect(ctx, upstream, client.NetworkDisconnectOptions{Container: c, Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cli.NetworkDisconnect(ctx, egress, client.NetworkDisconnectOptions{Container: inst.ID, Force: true}); err != nil {
		t.Fatal(err)
	}
	if out, err := sh("curl -fsS -m 5 -o /dev/null https://pypi.org/simple/ && echo erreichbar"); err == nil {
		t.Fatalf("(c) Sandbox erreicht pypi.org noch direkt: %s", out)
	}
	start = time.Now()
	if out, err := sh("pip uninstall -y iniconfig >/dev/null && pip install --no-deps --no-cache-dir iniconfig==2.0.0 2>&1 && python3 -c 'import iniconfig'"); err != nil {
		t.Fatalf("(c) pip install aus dem Zwischenspeicher: %v\n%s", err, out)
	}
	t.Logf("(c) pip install aus pip-cache ohne Upstream: %.1f s", time.Since(start).Seconds())
	start = time.Now()
	if out, err := sh("rm -rf ~/.npm /workspace/p && mkdir -p /workspace/p && cd /workspace/p && npm install --no-audit --no-fund is-number@7.0.0 2>&1 && node -e 'require(\"is-number\")'"); err != nil {
		t.Fatalf("(c) npm install aus dem Zwischenspeicher: %v\n%s", err, out)
	}
	t.Logf("(c) npm install aus npm-cache ohne Upstream: %.1f s", time.Since(start).Seconds())

	// (d) Abschalten löst die Zwischenspeicher vom Platz-Netz.
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatal(err)
	}
	ins, err := cli.NetworkInspect(ctx, slotNet, client.NetworkInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ins.Network.Containers); n != 1 {
		t.Fatalf("(d) Platz-Netz hat nach dem Abschalten %d Teilnehmer, erwartet 1 (nur die Sandbox)", n)
	}
	unreachable("(d) nach dem Abschalten")

	// Abbau mit angehängtem Zwischenspeicher: RemoveSlotNetwork löst ihn.
	if err := rt.SetInternet(ctx, inst.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	removed = true
	if err := rt.RemoveSlotNetwork(ctx, slotNet); err != nil {
		t.Fatalf("Platz-Netz mit angehängtem Zwischenspeicher nicht abbaubar: %v", err)
	}
}

// composeImage liest die Abbild-Angabe eines Dienstes aus compose.yaml, damit
// der Test dieselbe Fassung prüft wie der Betrieb.
func composeImage(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+image:\s+(` + regexp.QuoteMeta(repo) + `\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("kein Abbild %s in compose.yaml", repo)
	}
	return string(m[1])
}

func startCache(ctx context.Context, t *testing.T, cli *client.Client, name, image, net string, env []string, mounts []mount.Mount) {
	t.Helper()
	if _, err := cli.ImageInspect(ctx, image); err != nil {
		rc, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: image, Env: env},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(net), Mounts: mounts,
			ReadonlyRootfs: true, Tmpfs: map[string]string{"/tmp": ""},
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = cli.ContainerRemove(bg, res.ID, client.ContainerRemoveOptions{Force: true})
		for _, m := range mounts {
			if m.Type == mount.TypeVolume {
				_, _ = cli.VolumeRemove(bg, m.Source, client.VolumeRemoveOptions{Force: true})
			}
		}
	})
	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(fmt.Errorf("%s starten: %w", name, err))
	}
}

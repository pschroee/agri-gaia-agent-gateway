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

// TestPkgCacheIntegration checks the package caches with their own networks
// and their own Verdaccio/proxpi containers (images as in compose.yaml):
//
//	(a) without internet npm-cache and pip-cache are not reachable from the sandbox,
//	(b) with internet pip install and npm install go through the caches,
//	(c) a second fetch comes from the cache: for this the caches
//	    lose their internet and the sandbox its egress network,
//	(d) after switching off nothing is reachable again, and a slot network with
//	    an attached cache can be torn down.
//
// Runs only with AGW_DOCKER_TESTS=1 and needs internet on the host.
func TestPkgCacheIntegration(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
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
	if err := rt.EnsureEgressNetwork(ctx, TestSubnet()); err != nil { // without ICC as in production
		t.Fatal(err)
	}
	defer func() {
		for _, n := range []string{slotNet, egress, upstream} {
			_, _ = cli.NetworkRemove(bg, n, client.NetworkRemoveOptions{})
		}
	}()

	// Caches as in compose.yaml, but with their own volumes.
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
		// Not "true": a restart of the orchestrator (hot reload) would otherwise clean up the
		// test sandbox in the middle of the test (RemoveManaged).
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
			if out, err := sh("curl -fsS -m 5 -o /dev/null " + u + " && echo reachable"); err == nil {
				t.Fatalf("%s: %s reachable (%s)", when, u, out)
			}
		}
		start := time.Now()
		// pip download and an empty npm cache: both must ask the index,
		// even if the package is already installed.
		if out, err := sh("pip download --no-deps --no-cache-dir -d $(mktemp -d) iniconfig==2.0.0 2>&1"); err == nil {
			t.Fatalf("%s: pip download succeeded without internet: %s", when, out)
		}
		t.Logf("%s: pip download fails after %.1f s", when, time.Since(start).Seconds())
		start = time.Now()
		if out, err := sh("rm -rf ~/.npm && cd $(mktemp -d) && npm install --no-audit --no-fund is-number@7.0.0 2>&1"); err == nil {
			t.Fatalf("%s: npm install succeeded without internet: %s", when, out)
		}
		t.Logf("%s: npm install fails after %.1f s", when, time.Since(start).Seconds())
	}

	// (a) Without internet: no way to the caches.
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatal(err)
	}
	unreachable("(a) without internet")

	// (b) With internet: installation through the caches.
	if err := rt.SetInternet(ctx, inst.ID, true); err != nil {
		t.Fatal(err)
	}
	if out, err := sh(`for i in $(seq 1 60); do curl -fsS -m 2 -o /dev/null http://npm-cache:4873/-/ping && curl -fsS -m 2 -o /dev/null http://pip-cache:5000/ && exit 0; sleep 0.5; done; exit 1`); err != nil {
		t.Fatalf("(b) caches not reachable: %v %s", err, out)
	}
	start := time.Now()
	out, err := sh("pip install --no-deps --no-cache-dir iniconfig==2.0.0 2>&1")
	if err != nil || !strings.Contains(out, "pip-cache:5000") {
		t.Fatalf("(b) pip install: %v\n%s", err, out)
	}
	t.Logf("(b) pip install through pip-cache: %.1f s", time.Since(start).Seconds())
	start = time.Now()
	if out, err := sh("mkdir -p /workspace/p && cd /workspace/p && npm install --no-audit --no-fund is-number@7.0.0 2>&1 && grep -o 'npm-cache:4873[^\"]*' package-lock.json"); err != nil {
		t.Fatalf("(b) npm install: %v\n%s", err, out)
	}
	t.Logf("(b) npm install through npm-cache: %.1f s", time.Since(start).Seconds())

	// (c) Second fetch: caches without upstream, sandbox without egress.
	// If the installation still succeeds, it came from the cache.
	for _, c := range []string{npmName, pipName} {
		if _, err := cli.NetworkDisconnect(ctx, upstream, client.NetworkDisconnectOptions{Container: c, Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cli.NetworkDisconnect(ctx, egress, client.NetworkDisconnectOptions{Container: inst.ID, Force: true}); err != nil {
		t.Fatal(err)
	}
	if out, err := sh("curl -fsS -m 5 -o /dev/null https://pypi.org/simple/ && echo reachable"); err == nil {
		t.Fatalf("(c) sandbox still reaches pypi.org directly: %s", out)
	}
	start = time.Now()
	if out, err := sh("pip uninstall -y iniconfig >/dev/null && pip install --no-deps --no-cache-dir iniconfig==2.0.0 2>&1 && python3 -c 'import iniconfig'"); err != nil {
		t.Fatalf("(c) pip install from the cache: %v\n%s", err, out)
	}
	t.Logf("(c) pip install from pip-cache without upstream: %.1f s", time.Since(start).Seconds())
	start = time.Now()
	if out, err := sh("rm -rf ~/.npm /workspace/p && mkdir -p /workspace/p && cd /workspace/p && npm install --no-audit --no-fund is-number@7.0.0 2>&1 && node -e 'require(\"is-number\")'"); err != nil {
		t.Fatalf("(c) npm install from the cache: %v\n%s", err, out)
	}
	t.Logf("(c) npm install from npm-cache without upstream: %.1f s", time.Since(start).Seconds())

	// (d) Switching off detaches the caches from the slot network.
	if err := rt.SetInternet(ctx, inst.ID, false); err != nil {
		t.Fatal(err)
	}
	ins, err := cli.NetworkInspect(ctx, slotNet, client.NetworkInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ins.Network.Containers); n != 1 {
		t.Fatalf("(d) slot network has %d members after switching off, expected 1 (only the sandbox)", n)
	}
	unreachable("(d) after switching off")

	// Teardown with an attached cache: RemoveSlotNetwork detaches it.
	if err := rt.SetInternet(ctx, inst.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	removed = true
	if err := rt.RemoveSlotNetwork(ctx, slotNet); err != nil {
		t.Fatalf("slot network with attached cache cannot be torn down: %v", err)
	}
}

// composeImage reads a service's image from compose.yaml so that
// the test checks the same version as production.
func composeImage(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+image:\s+(` + regexp.QuoteMeta(repo) + `\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no image %s in compose.yaml", repo)
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
		t.Fatal(fmt.Errorf("starting %s: %w", name, err))
	}
}

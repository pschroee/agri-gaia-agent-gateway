// Package sandbox startet gehärtete Docker-Container mit pi (E1, Stufe 1).
// Die Sandbox hängt immer am internen Netz ohne Ausgang, über das sie nur den
// LLM-Proxy des Orchestrators erreicht. Internetzugang entsteht allein durch
// Verbinden mit dem Egress-Netz und lässt sich zur Laufzeit wieder trennen.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	LabelManaged = "agwpoc.managed"
	LabelSlot    = "agwpoc.slot"
	LabelVariant = "agwpoc.variant"
	LabelRole    = "agwpoc.role" // pi | exec (E9)
	agentUID     = "10001"
)

type Spec struct {
	Name          string
	Image         string
	Args          []string // Argumente für pi (nach dem Entrypoint)
	Env           []string
	Labels        map[string]string
	InternalNet   string // immer verbunden, ohne Ausgang
	SocketVolume  string // benanntes Volume mit den Socket-Verzeichnissen
	SocketSubpath string // Unterverzeichnis dieses Platzes
	MemoryMB      int64
	CPUs          float64
	Pids          int64
	// Tmpfs ersetzt die voreingestellten tmpfs-Einhängungen (nil: DefaultTmpfs).
	Tmpfs map[string]string
	// NoAttach: stdin/stdout nicht anhängen (Ausführungs-Sandbox, PID 1 ist agw-exec idle).
	NoAttach bool
	// CapAdd: Capabilities zusätzlich zu "keine". Die Ausführungs-Sandbox braucht
	// SETUID/SETGID, damit ihr Überwacher (root, per exec) Operationen als
	// Agent-Nutzer starten kann; Prozesse des Agenten haben sie nie (E9).
	CapAdd []string
}

const tmpfsAgent = "uid=" + agentUID + ",gid=" + agentUID + ",mode=0755"

// Docker hängt tmpfs sonst mit noexec ein: Skripte und kompilierte
// Bibliotheken (numpy-.so in ~/.local) ließen sich dann nicht laden. noexec
// schützt hier nichts, weil der Agent mit bash ohnehin Code ausführen darf.
const tmpfsExec = tmpfsAgent + ",exec"

// DefaultTmpfs: Stufe 1, pi und Agent in einem Container.
var DefaultTmpfs = map[string]string{
	"/agent":      tmpfsAgent + ",size=512m",
	"/workspace":  tmpfsExec + ",size=1g",
	"/home/agent": tmpfsExec + ",size=1g",
	"/tmp":        "mode=1777,exec,size=512m",
}

// PiTmpfs: Container von pi nach E9. Kein /workspace (das Abbild bringt ein
// leeres, schreibgeschütztes Verzeichnis als Arbeitsverzeichnis von pi mit).
var PiTmpfs = map[string]string{
	"/agent":      tmpfsAgent + ",size=512m",
	"/home/agent": tmpfsAgent + ",size=64m",
	"/tmp":        "mode=1777,size=256m",
}

// ExecCaps: Capabilities der Ausführungs-Sandbox. Nur ihr Überwacher (root per docker exec)
// hat sie; Prozesse des Agenten (uid 10001, no-new-privileges) haben keine. SETUID/SETGID zum
// Start jeder Operation als Agent-Nutzer, KILL für die Notbremse bei erschöpftem PidsLimit (N3).
var ExecCaps = []string{"SETUID", "SETGID", "KILL"}

// ExecTmpfs: Ausführungs-Sandbox nach E9. Kein /agent.
var ExecTmpfs = map[string]string{
	"/workspace":  tmpfsExec + ",size=1g",
	"/home/agent": tmpfsExec + ",size=1g",
	"/tmp":        "mode=1777,exec,size=512m",
}

type Instance struct {
	ID     string
	Name   string
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr *TailBuffer
	done   chan struct{}
}

// Done wird geschlossen, wenn der Ausgabestrom des Containers endet.
func (i *Instance) Done() <-chan struct{} { return i.done }

type Runtime struct {
	cli       *client.Client
	egressNet string
	self      string     // eigener Container des Orchestrators (für Platz-Netze)
	caches    []PkgCache // Paket-Zwischenspeicher, nur bei Internet erreichbar
}

// PkgCache ist ein Paket-Zwischenspeicher (Container), den eine Sandbox unter
// einem festen Namen erreicht, aber nur, solange sie Internet hat.
type PkgCache struct {
	Container string // Name oder ID des Containers
	Alias     string // Name im Platz-Netz der Sandbox
}

const (
	NpmCacheAlias = "npm-cache"
	PipCacheAlias = "pip-cache"
	npmCacheURL   = "http://" + NpmCacheAlias + ":4873/"
	pipCacheURL   = "http://" + PipCacheAlias + ":5000/index/"
)

// SetPkgCaches nennt die Container der Zwischenspeicher für npm (Verdaccio)
// und pip (proxpi). Leere Namen schalten den jeweiligen Zwischenspeicher ab.
func (r *Runtime) SetPkgCaches(npmContainer, pipContainer string) {
	r.caches = nil
	if npmContainer != "" {
		r.caches = append(r.caches, PkgCache{Container: npmContainer, Alias: NpmCacheAlias})
	}
	if pipContainer != "" {
		r.caches = append(r.caches, PkgCache{Container: pipContainer, Alias: PipCacheAlias})
	}
}

// PkgCacheEnv liefert die Umgebung, mit der npm und pip in der Sandbox die
// Zwischenspeicher verwenden. Ohne Internet ist der Name nicht auflösbar und
// die Installation scheitert: pip nach rund 8 s (fünf Wiederholungen), npm
// dank kurzer Wiederholung nach rund 2 s statt 70 s.
func (r *Runtime) PkgCacheEnv() []string {
	var env []string
	for _, c := range r.caches {
		switch c.Alias {
		case NpmCacheAlias:
			env = append(env, "NPM_CONFIG_REGISTRY="+npmCacheURL,
				"NPM_CONFIG_FETCH_RETRIES=1", "NPM_CONFIG_FETCH_RETRY_MINTIMEOUT=2000", "NPM_CONFIG_FETCH_RETRY_MAXTIMEOUT=5000")
		case PipCacheAlias:
			env = append(env, "PIP_INDEX_URL="+pipCacheURL, "PIP_TRUSTED_HOST="+PipCacheAlias)
		}
	}
	return env
}

func New(egressNet string) (*Runtime, error) {
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Runtime{cli: cli, egressNet: egressNet}, nil
}

// SetSelf nennt den Container des Orchestrators. Er wird an jedes Platz-Netz
// gehängt, damit die Sandbox den LLM-Proxy erreicht.
func (r *Runtime) SetSelf(container string) { r.self = container }

const (
	LabelSlotNet  = "agwpoc.slotnet"
	slotNetPrefix = "agwpoc_slot_"
)

// CreateSlotNetwork legt ein internes Netz nur für diesen Platz an und hängt
// den Orchestrator mit dem Alias "orchestrator" daran. In dem Netz gibt es
// genau zwei Teilnehmer; Sandboxen erreichen sich so nicht gegenseitig
// (Review H5). Das Subnetz (/28) wird zufällig aus 10.231.128.0/17 gewählt.
func (r *Runtime) CreateSlotNetwork(ctx context.Context, slotID string) (string, error) {
	return r.createNetwork(ctx, slotID, slotNetPrefix+slotID, true)
}

// CreateExecNetwork legt das Netz der Ausführungs-Sandbox an (E9): intern,
// ohne Orchestrator und ohne Container von pi. Ohne Internet ist die
// Ausführungs-Sandbox darin allein; mit Internet hängen hier die
// Paket-Zwischenspeicher. Den LLM-Proxy erreicht sie nicht.
func (r *Runtime) CreateExecNetwork(ctx context.Context, slotID string) (string, error) {
	return r.createNetwork(ctx, slotID, slotNetPrefix+slotID+"_x", false)
}

func (r *Runtime) createNetwork(ctx context.Context, slotID, name string, withSelf bool) (string, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		n := int(b[0])<<8 | int(b[1])
		n %= 2048 // 2048 × /28 in 10.231.128.0/17
		subnet := fmt.Sprintf("10.231.%d.%d/28", 128+n/16, (n%16)*16)
		_, err := r.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
			Driver: "bridge", Internal: true,
			Labels: map[string]string{LabelManaged: "true", LabelSlotNet: slotID},
			IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: mustPrefix(subnet)}}},
		})
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if !strings.Contains(err.Error(), "overlap") && !strings.Contains(err.Error(), "Pool") {
			return "", err
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("Platz-Netz: %w", lastErr)
	}
	if r.self != "" && withSelf {
		_, err := r.cli.NetworkConnect(ctx, name, client.NetworkConnectOptions{
			Container: r.self, EndpointConfig: &network.EndpointSettings{Aliases: []string{"orchestrator"}},
		})
		if err != nil {
			_, _ = r.cli.NetworkRemove(context.WithoutCancel(ctx), name, client.NetworkRemoveOptions{})
			return "", fmt.Errorf("Orchestrator an Platz-Netz hängen: %w", err)
		}
	}
	return name, nil
}

// RemoveSlotNetwork löst den Orchestrator vom Platz-Netz und entfernt es.
// Hängt noch ein Zwischenspeicher daran (Sandbox mit Internet abgebaut),
// wird er ebenfalls gelöst.
func (r *Runtime) RemoveSlotNetwork(ctx context.Context, name string) error {
	if r.self != "" {
		_, _ = r.cli.NetworkDisconnect(ctx, name, client.NetworkDisconnectOptions{Container: r.self, Force: true})
	}
	for _, c := range r.caches {
		_, _ = r.cli.NetworkDisconnect(ctx, name, client.NetworkDisconnectOptions{Container: c.Container, Force: true})
	}
	_, err := r.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	return err
}

func (r *Runtime) Client() *client.Client { return r.cli }

// Start legt den Container an, hängt sich an stdin/stdout und startet ihn.
func (r *Runtime) Start(ctx context.Context, s Spec) (*Instance, error) {
	pids := s.Pids
	labels := map[string]string{LabelManaged: "true"}
	for k, v := range s.Labels {
		labels[k] = v
	}
	tmpfs := s.Tmpfs
	if tmpfs == nil {
		tmpfs = DefaultTmpfs
	}
	hc := &container.HostConfig{
		NetworkMode:    container.NetworkMode(s.InternalNet),
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		CapAdd:         s.CapAdd,
		SecurityOpt:    []string{"no-new-privileges:true"},
		Tmpfs:          tmpfs,
		Resources: container.Resources{
			Memory:    s.MemoryMB << 20,
			NanoCPUs:  int64(s.CPUs * 1e9),
			PidsLimit: &pids,
		},
	}
	if s.SocketVolume != "" {
		hc.Mounts = []mount.Mount{{
			Type:          mount.TypeVolume,
			Source:        s.SocketVolume,
			Target:        "/run/agw",
			ReadOnly:      true,
			VolumeOptions: &mount.VolumeOptions{Subpath: s.SocketSubpath, NoCopy: true},
		}}
	}
	cfg := &container.Config{
		Image:        s.Image,
		Cmd:          s.Args,
		Env:          s.Env,
		User:         agentUID + ":" + agentUID,
		Labels:       labels,
		OpenStdin:    !s.NoAttach,
		AttachStdin:  !s.NoAttach,
		AttachStdout: !s.NoAttach,
		AttachStderr: !s.NoAttach,
		StdinOnce:    false,
		Tty:          false,
	}
	res, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hc, Name: s.Name})
	if err != nil {
		return nil, fmt.Errorf("Container anlegen: %w", err)
	}
	id := res.ID
	if s.NoAttach {
		if _, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
			_ = r.Remove(context.WithoutCancel(ctx), id)
			return nil, fmt.Errorf("Container starten: %w", err)
		}
		done := make(chan struct{})
		go r.watch(id, done)
		return &Instance{ID: id, Name: s.Name, Stderr: NewTailBuffer(1), done: done}, nil
	}
	att, err := r.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		_ = r.Remove(context.WithoutCancel(ctx), id)
		return nil, fmt.Errorf("an Container anhängen: %w", err)
	}
	outR, outW := io.Pipe()
	errBuf := NewTailBuffer(16 << 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := stdcopy.StdCopy(outW, errBuf, att.Reader)
		outW.CloseWithError(err)
		att.Close()
	}()
	if _, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		att.Close()
		_ = r.Remove(context.WithoutCancel(ctx), id)
		return nil, fmt.Errorf("Container starten: %w", err)
	}
	return &Instance{ID: id, Name: s.Name, Stdin: &hijackStdin{h: &att.HijackedResponse}, Stdout: outR, Stderr: errBuf, done: done}, nil
}

// watch schließt done, sobald der Container nicht mehr läuft (Container ohne angehängte
// Ströme, also die Ausführungs-Sandbox; H2). Reißt die Verbindung zu Docker ab, fragt es
// nach, statt ein Ende zu melden, das keines ist.
func (r *Runtime) watch(id string, done chan struct{}) {
	defer close(done)
	for {
		wr := r.cli.ContainerWait(context.Background(), id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		select {
		case <-wr.Result:
			return
		case err := <-wr.Error:
			if err == nil {
				return
			}
		}
		time.Sleep(2 * time.Second)
		ins, err := r.cli.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return
			}
			continue
		}
		if ins.Container.State == nil || !ins.Container.State.Running {
			return
		}
	}
}

type hijackStdin struct {
	h    *client.HijackedResponse
	once sync.Once
}

func (w *hijackStdin) Write(p []byte) (int, error) { return w.h.Conn.Write(p) }
func (w *hijackStdin) Close() error {
	var err error
	w.once.Do(func() { err = w.h.CloseWrite() })
	return err
}

// Exec führt ein Kommando als Agent-Nutzer im Container aus. Die Sitzungs-
// dateien liegen im tmpfs; `docker cp` sieht tmpfs-Inhalte nicht, exec schon.
func (r *Runtime) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) ([]byte, int, error) {
	ex, err := r.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: agentUID + ":" + agentUID, Cmd: cmd,
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, -1, err
	}
	att, err := r.cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, -1, err
	}
	defer att.Close()
	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite()
		}()
	}
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, att.Reader); err != nil {
		return nil, -1, err
	}
	ins, err := r.cli.ExecInspect(ctx, ex.ID, client.ExecInspectOptions{})
	if err != nil {
		return nil, -1, err
	}
	if ins.ExitCode != 0 {
		return out.Bytes(), ins.ExitCode, fmt.Errorf("exec %v: Exit-Code %d: %s", cmd, ins.ExitCode, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), 0, nil
}

// ExecStream startet ein langlebiges Kommando im Container (als user, etwa
// "0:0" für den Überwacher der Ausführungs-Sandbox) und liefert stdin und
// stdout; stderr landet in einem Puffer für Fehlermeldungen. close beendet
// die Verbindung (einmalig).
func (r *Runtime) ExecStream(ctx context.Context, id string, cmd []string, user string) (io.WriteCloser, io.Reader, func(), *TailBuffer, error) {
	ex, err := r.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: user, Cmd: cmd, AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	att, err := r.cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	outR, outW := io.Pipe()
	errBuf := NewTailBuffer(8 << 10)
	go func() {
		_, err := stdcopy.StdCopy(outW, errBuf, att.Reader)
		outW.CloseWithError(err)
	}()
	var once sync.Once
	closeFn := func() { once.Do(func() { att.Close(); _ = outR.Close() }) }
	return &hijackStdin{h: &att.HijackedResponse}, outR, closeFn, errBuf, nil
}

// ContainerIP liefert die Adresse des Containers in einem Netz.
func (r *Runtime) ContainerIP(ctx context.Context, id, netName string) (string, error) {
	res, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if res.Container.NetworkSettings == nil {
		return "", errors.New("keine Netzangaben")
	}
	ep, ok := res.Container.NetworkSettings.Networks[netName]
	if !ok || ep == nil || !ep.IPAddress.IsValid() {
		return "", fmt.Errorf("Container nicht im Netz %s", netName)
	}
	return ep.IPAddress.String(), nil
}

// SetInternet verbindet den Container mit dem Egress-Netz oder trennt ihn.
// Die Paket-Zwischenspeicher folgen dem Schalter: Mit Internet hängt sie der
// Orchestrator unter ihrem Alias an das Platz-Netz der Sandbox, ohne Internet
// löst er sie wieder. Ins Egress-Netz können sie nicht, weil es ohne ICC
// Verkehr zwischen Containern verwirft. Ohne Internet gibt es damit weiterhin
// keinen Weg nach draußen, auch nicht über einen Zwischenspeicher.
func (r *Runtime) SetInternet(ctx context.Context, id string, on bool) error {
	if r.egressNet == "" {
		return errors.New("kein Egress-Netz konfiguriert")
	}
	if on {
		_, err := r.cli.NetworkConnect(ctx, r.egressNet, client.NetworkConnectOptions{Container: id})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
		r.attachCaches(ctx, id)
		return nil
	}
	_, err := r.cli.NetworkDisconnect(ctx, r.egressNet, client.NetworkDisconnectOptions{Container: id, Force: true})
	if err != nil && !notConnected(err) {
		return err
	}
	return r.detachCaches(ctx, id)
}

func notConnected(err error) bool {
	m := err.Error()
	return strings.Contains(m, "not connected") || strings.Contains(m, "No such container") || strings.Contains(m, "not found")
}

// sandboxNets liefert die Netze des Containers außer dem Egress-Netz, also
// sein Platz-Netz.
func (r *Runtime) sandboxNets(ctx context.Context, id string) ([]string, error) {
	res, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	var nets []string
	if res.Container.NetworkSettings != nil {
		for n := range res.Container.NetworkSettings.Networks {
			if n != r.egressNet {
				nets = append(nets, n)
			}
		}
	}
	return nets, nil
}

// attachCaches hängt die Zwischenspeicher an das Platz-Netz. Fehlt einer
// (Dienst nicht gestartet), bleibt das Internet trotzdem an; Installationen
// über ihn scheitern dann.
func (r *Runtime) attachCaches(ctx context.Context, id string) {
	if len(r.caches) == 0 {
		return
	}
	nets, err := r.sandboxNets(ctx, id)
	if err != nil {
		slog.Warn("Paket-Zwischenspeicher nicht angehängt", "fehler", err)
		return
	}
	for _, n := range nets {
		for _, c := range r.caches {
			_, err := r.cli.NetworkConnect(ctx, n, client.NetworkConnectOptions{
				Container: c.Container, EndpointConfig: &network.EndpointSettings{Aliases: []string{c.Alias}},
			})
			if err != nil && !strings.Contains(err.Error(), "already exists") {
				slog.Warn("Paket-Zwischenspeicher nicht angehängt", "container", c.Container, "netz", n, "fehler", err)
			}
		}
	}
}

// detachCaches löst die Zwischenspeicher vom Platz-Netz. Scheitert das, meldet
// es einen Fehler: Ein verbliebener Zwischenspeicher wäre ein Weg nach draußen.
func (r *Runtime) detachCaches(ctx context.Context, id string) error {
	if len(r.caches) == 0 {
		return nil
	}
	nets, err := r.sandboxNets(ctx, id)
	if err != nil {
		return fmt.Errorf("Paket-Zwischenspeicher lösen: %w", err)
	}
	var errs []error
	for _, n := range nets {
		for _, c := range r.caches {
			_, err := r.cli.NetworkDisconnect(ctx, n, client.NetworkDisconnectOptions{Container: c.Container, Force: true})
			if err != nil && !notConnected(err) {
				errs = append(errs, fmt.Errorf("%s von %s lösen: %w", c.Container, n, err))
			}
		}
	}
	return errors.Join(errs...)
}

// EnsureEgressNetwork legt das Egress-Netz an, falls es fehlt. Compose legt
// es nicht an, weil kein Dienst daran hängt; der Orchestrator selbst soll
// es auch nicht, damit die Sandboxen darüber keinen Weg zu ihm haben.
func (r *Runtime) EnsureEgressNetwork(ctx context.Context, subnet string) error {
	if ins, err := r.cli.NetworkInspect(ctx, r.egressNet, client.NetworkInspectOptions{}); err == nil {
		if ins.Network.Options[iccOption] == "false" {
			return nil
		}
		// Älteres Egress-Netz mit ICC: ersetzen, solange nichts daran hängt.
		if len(ins.Network.Containers) > 0 {
			return fmt.Errorf("Egress-Netz %s erlaubt Verkehr zwischen Containern und ist belegt; ./dev.sh stop und erneut starten", r.egressNet)
		}
		if _, err := r.cli.NetworkRemove(ctx, r.egressNet, client.NetworkRemoveOptions{}); err != nil {
			return err
		}
	}
	// Ohne ICC erreichen sich Sandboxen mit Internet nicht gegenseitig (Review H5).
	opts := client.NetworkCreateOptions{Driver: "bridge", Labels: map[string]string{LabelManaged: "true"},
		Options: map[string]string{iccOption: "false"}}
	if subnet != "" {
		opts.IPAM = &network.IPAM{Config: []network.IPAMConfig{{Subnet: mustPrefix(subnet)}}}
	}
	_, err := r.cli.NetworkCreate(ctx, r.egressNet, opts)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (r *Runtime) Remove(ctx context.Context, id string) error {
	_, err := r.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

const iccOption = "com.docker.network.bridge.enable_icc"

// RemoveSlotNetworks räumt Platz-Netze früherer Läufe ab.
func (r *Runtime) RemoveSlotNetworks(ctx context.Context) int {
	res, err := r.cli.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("label", LabelSlotNet)})
	if err != nil {
		return 0
	}
	n := 0
	for _, nw := range res.Items {
		if r.RemoveSlotNetwork(ctx, nw.Name) == nil {
			n++
		}
	}
	return n
}

// RemoveManaged räumt Container früherer Läufe des Orchestrators ab.
func (r *Runtime) RemoveManaged(ctx context.Context) (int, error) {
	res, err := r.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", LabelManaged+"=true")})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range res.Items {
		if err := r.Remove(ctx, c.ID); err == nil {
			n++
		}
	}
	return n, nil
}

// TailBuffer hält die letzten n Bytes (stderr von pi, für Fehlermeldungen).
type TailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func NewTailBuffer(max int) *TailBuffer { return &TailBuffer{max: max} }

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

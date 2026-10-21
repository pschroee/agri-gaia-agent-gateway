// Package sandbox starts hardened Docker containers with pi (E1, stage 1).
// The sandbox is always attached to the internal network without egress, through which it
// reaches only the orchestrator's LLM proxy. Internet access comes solely from
// connecting to the egress network and can be disconnected again at runtime.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
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
	Name  string
	Image string
	// Args: command of the container; without Entrypoint these are the arguments of the image's
	// entrypoint (as for pi: agw-exec pi-entry <args>).
	Args []string
	// Entrypoint replaces the entrypoint of the image; empty keeps it. The check container (R0) sets it
	// to the check script: the execution image's entrypoint is `agw-exec idle`, which ignores arguments
	// and never exits.
	Entrypoint    []string
	Env           []string
	Labels        map[string]string
	InternalNet   string // always connected, without egress
	SocketVolume  string // named volume with the socket directories
	SocketSubpath string // subdirectory of this slot
	MemoryMB      int64
	CPUs          float64
	Pids          int64
	// Tmpfs replaces the default tmpfs mounts (nil: DefaultTmpfs).
	Tmpfs map[string]string
	// NoAttach: do not attach stdin/stdout (execution sandbox, PID 1 is agw-exec idle).
	NoAttach bool
	// CapAdd: capabilities in addition to "none". The execution sandbox needs
	// SETUID/SETGID so that its supervisor (root, via exec) can start operations as
	// the agent user; the agent's processes never have them (E9).
	CapAdd []string
}

const tmpfsAgent = "uid=" + agentUID + ",gid=" + agentUID + ",mode=0755"

// Otherwise Docker mounts tmpfs with noexec: scripts and compiled
// libraries (numpy .so in ~/.local) could then not be loaded. noexec
// protects nothing here, because the agent may run code with bash anyway.
const tmpfsExec = tmpfsAgent + ",exec"

// DefaultTmpfs: stage 1, pi and agent in one container.
var DefaultTmpfs = map[string]string{
	"/agent":      tmpfsAgent + ",size=512m",
	"/workspace":  tmpfsExec + ",size=1g",
	"/home/agent": tmpfsExec + ",size=1g",
	"/tmp":        "mode=1777,exec,size=512m",
}

// PiTmpfs: pi's container after E9. No /workspace (the image brings an
// empty, read-only directory as pi's working directory).
var PiTmpfs = map[string]string{
	"/agent":      tmpfsAgent + ",size=512m",
	"/home/agent": tmpfsAgent + ",size=64m",
	"/tmp":        "mode=1777,size=256m",
}

// ExecCaps: capabilities of the execution sandbox. Only its supervisor (root via docker exec)
// has them; the agent's processes (uid 10001, no-new-privileges) have none. SETUID/SETGID to
// start every operation as the agent user, KILL for the emergency brake on an exhausted PidsLimit (N3).
var ExecCaps = []string{"SETUID", "SETGID", "KILL"}

// TraceDir: root-only directory of the syscall traces (X6). The supervisor writes one
// file per operation; the agent (uid 10001) can neither read nor write it. It lives in a
// tmpfs, so a restart of the slot leaves no traces behind.
const TraceDir = "/var/log/agw-trace"

// ExecTmpfs: execution sandbox after E9. No /agent.
var ExecTmpfs = map[string]string{
	"/workspace":  tmpfsExec + ",size=1g",
	"/home/agent": tmpfsExec + ",size=1g",
	"/tmp":        "mode=1777,exec,size=512m",
	TraceDir:      "mode=0700,uid=0,gid=0,size=64m",
}

type Instance struct {
	ID   string
	Name string
	// ImageID is the Docker image ID (sha256:…) the container actually runs (X8.2). It is read once at
	// start, so a slot that still carries an older image after a rebuild is visible in the data.
	ImageID string
	Stdin   io.WriteCloser
	Stdout  io.Reader
	Stderr  *TailBuffer
	done    chan struct{}
}

// Done is closed when the container's output stream ends.
func (i *Instance) Done() <-chan struct{} { return i.done }

type Runtime struct {
	cli       *client.Client
	egressNet string
	self      string // the orchestrator's own container (for slot networks)
}

// RelayAddr is where the relay in the execution sandbox listens (agw-exec serve); pip, npm and every program that
// honours the proxy variables reach the gateway through it and through nothing else (issue #175).
const RelayAddr = "127.0.0.1:3128"

// RelayEnv returns the environment of the execution sandbox that points pip, npm and the proxy-aware programs at
// the relay. The relay is the only way out: without it, or without a grant that covers the target, nothing is
// reachable (the sandbox's network has no route; fail closed). Both spellings of the proxy variables are set
// because git and curl read the lower case ones.
func RelayEnv() []string {
	proxy := "http://" + RelayAddr
	return []string{
		"http_proxy=" + proxy, "https_proxy=" + proxy, "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy,
		"no_proxy=127.0.0.1,localhost", "NO_PROXY=127.0.0.1,localhost",
		"PIP_INDEX_URL=" + proxy + "/index/",
		"NPM_CONFIG_REGISTRY=" + proxy + "/npm/",
		"NPM_CONFIG_AUDIT=false", "NPM_CONFIG_FUND=false",
		"NPM_CONFIG_FETCH_RETRIES=1", "NPM_CONFIG_FETCH_RETRY_MINTIMEOUT=2000", "NPM_CONFIG_FETCH_RETRY_MAXTIMEOUT=5000",
	}
}

func New(egressNet string) (*Runtime, error) {
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Runtime{cli: cli, egressNet: egressNet}, nil
}

// SetSelf names the orchestrator's container. It is attached to every slot network
// so that the sandbox reaches the LLM proxy.
func (r *Runtime) SetSelf(container string) { r.self = container }

const (
	LabelSlotNet  = "agwpoc.slotnet"
	slotNetPrefix = "agwpoc_slot_"
)

// CreateSlotNetwork creates an internal network just for this slot and attaches
// the orchestrator to it with the alias "orchestrator". The network has
// exactly two members; this way sandboxes cannot reach each other
// (Review H5). The subnet (/28) is chosen at random from 10.231.128.0/17.
func (r *Runtime) CreateSlotNetwork(ctx context.Context, slotID string) (string, error) {
	return r.createNetwork(ctx, slotID, slotNetPrefix+slotID, true)
}

// CreateExecNetwork creates the execution sandbox's network (E9): internal,
// without orchestrator and without pi's container. Without internet the
// execution sandbox is alone in it; with internet the
// package caches are attached here. It does not reach the LLM proxy.
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
		return "", fmt.Errorf("slot network: %w", lastErr)
	}
	if r.self != "" && withSelf {
		_, err := r.cli.NetworkConnect(ctx, name, client.NetworkConnectOptions{
			Container: r.self, EndpointConfig: &network.EndpointSettings{Aliases: []string{"orchestrator"}},
		})
		if err != nil {
			_, _ = r.cli.NetworkRemove(context.WithoutCancel(ctx), name, client.NetworkRemoveOptions{})
			return "", fmt.Errorf("attaching orchestrator to slot network: %w", err)
		}
	}
	return name, nil
}

// RemoveSlotNetwork detaches the orchestrator from the slot network and removes it.
func (r *Runtime) RemoveSlotNetwork(ctx context.Context, name string) error {
	if r.self != "" {
		_, _ = r.cli.NetworkDisconnect(ctx, name, client.NetworkDisconnectOptions{Container: r.self, Force: true})
	}
	_, err := r.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	return err
}

func (r *Runtime) Client() *client.Client { return r.cli }

// Start creates the container, attaches to stdin/stdout and starts it.
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
		Entrypoint:   s.Entrypoint,
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
		return nil, fmt.Errorf("creating container: %w", err)
	}
	id := res.ID
	// X8.2: keep the image ID the container runs, not only the tag it was created from. After a rebuild,
	// a slot that was started earlier keeps the old image; the experiment data has to show that.
	imageID := r.imageID(ctx, id)
	if s.NoAttach {
		if _, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
			_ = r.Remove(context.WithoutCancel(ctx), id)
			return nil, fmt.Errorf("starting container: %w", err)
		}
		done := make(chan struct{})
		go r.watch(id, done)
		return &Instance{ID: id, Name: s.Name, ImageID: imageID, Stderr: NewTailBuffer(1), done: done}, nil
	}
	att, err := r.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		_ = r.Remove(context.WithoutCancel(ctx), id)
		return nil, fmt.Errorf("attaching to container: %w", err)
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
		return nil, fmt.Errorf("starting container: %w", err)
	}
	return &Instance{ID: id, Name: s.Name, ImageID: imageID, Stdin: &hijackStdin{h: &att.HijackedResponse}, Stdout: outR, Stderr: errBuf, done: done}, nil
}

// imageID reads the Docker image ID of a just-created container. A failure leaves the ID empty instead of
// stopping the slot; the experiment then records an empty value and the lock check reports it.
func (r *Runtime) imageID(ctx context.Context, id string) string {
	ins, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ""
	}
	return ins.Container.Image
}

// watch closes done as soon as the container is no longer running (containers without attached
// streams, i.e. the execution sandbox; H2). If the connection to Docker breaks, it checks
// again instead of reporting an end that is none.
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

// Exec runs a command as the agent user in the container. The session
// files live in tmpfs; `docker cp` does not see tmpfs contents, exec does.
func (r *Runtime) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) ([]byte, int, error) {
	return r.ExecAs(ctx, id, agentUID+":"+agentUID, cmd, stdin)
}

// ExecAs runs a command in the container as the given user (uid:gid), e.g. "1000:100" for the
// agent's notebook server (own code C-E3, whose image runs as jovyan). The session files live in
// tmpfs; `docker cp` does not see tmpfs contents, exec does.
func (r *Runtime) ExecAs(ctx context.Context, id, user string, cmd []string, stdin io.Reader) ([]byte, int, error) {
	ex, err := r.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: user, Cmd: cmd,
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
		return out.Bytes(), ins.ExitCode, fmt.Errorf("exec %v: exit code %d: %s", cmd, ins.ExitCode, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), 0, nil
}

// ExecStream starts a long-lived command in the container (as user, e.g.
// "0:0" for the execution sandbox's supervisor) and returns stdin and
// stdout; stderr ends up in a buffer for error messages. close ends
// the connection (once).
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

// ContainerIP returns the container's address in a network.
func (r *Runtime) ContainerIP(ctx context.Context, id, netName string) (string, error) {
	res, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if res.Container.NetworkSettings == nil {
		return "", errors.New("no network settings")
	}
	ep, ok := res.Container.NetworkSettings.Networks[netName]
	if !ok || ep == nil || !ep.IPAddress.IsValid() {
		return "", fmt.Errorf("container not in network %s", netName)
	}
	return ep.IPAddress.String(), nil
}

// SetInternet connects the container to the egress network or disconnects it. Only a full grant does that; every
// other kind goes through the relay and the gateway, which need no network route of their own. The package caches
// no longer join the slot network: the package gate in the gateway speaks to them (issue #175).
func (r *Runtime) SetInternet(ctx context.Context, id string, on bool) error {
	if r.egressNet == "" {
		return errors.New("no egress network configured")
	}
	if on {
		_, err := r.cli.NetworkConnect(ctx, r.egressNet, client.NetworkConnectOptions{Container: id})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
		return nil
	}
	_, err := r.cli.NetworkDisconnect(ctx, r.egressNet, client.NetworkDisconnectOptions{Container: id, Force: true})
	if err != nil && !notConnected(err) {
		return err
	}
	return nil
}

// NetBytes returns the bytes the container's network interfaces have received and sent together (Docker's counters;
// the count of a full internet grant, issue #175). A container that restarts begins at zero again.
func (r *Runtime) NetBytes(ctx context.Context, id string) (int64, error) {
	res, err := r.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var st struct {
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		} `json:"networks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return 0, err
	}
	var total uint64
	for _, n := range st.Networks {
		total += n.RxBytes + n.TxBytes
	}
	return int64(total), nil
}

func notConnected(err error) bool {
	m := err.Error()
	return strings.Contains(m, "not connected") || strings.Contains(m, "No such container") || strings.Contains(m, "not found")
}

// EnsureEgressNetwork creates the egress network if it is missing. Compose does not
// create it because no service is attached to it; the orchestrator itself should not be
// attached either, so that the sandboxes have no way to it through it.
func (r *Runtime) EnsureEgressNetwork(ctx context.Context, subnet string) error {
	if ins, err := r.cli.NetworkInspect(ctx, r.egressNet, client.NetworkInspectOptions{}); err == nil {
		if ins.Network.Options[iccOption] == "false" {
			return nil
		}
		// Older egress network with ICC: replace it as long as nothing is attached.
		if len(ins.Network.Containers) > 0 {
			return fmt.Errorf("egress network %s allows traffic between containers and is in use; run ./dev.sh stop and start again", r.egressNet)
		}
		if _, err := r.cli.NetworkRemove(ctx, r.egressNet, client.NetworkRemoveOptions{}); err != nil {
			return err
		}
	}
	// Without ICC sandboxes with internet cannot reach each other (Review H5).
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

// RemoveSlotNetworks cleans up slot networks of earlier runs.
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

// RemoveManaged cleans up containers of earlier orchestrator runs.
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

// FindByLabels returns the ids of the running containers that carry all these labels (own code
// C-E3 finds a chat's notebook server this way; the gateway sets the name, the agent never does).
// A label nobody matches gives no error and no id.
func (r *Runtime) FindByLabels(ctx context.Context, labels map[string]string) ([]string, error) {
	f := make(client.Filters)
	for k, v := range labels {
		f = f.Add("label", k+"="+v)
	}
	res, err := r.cli.ContainerList(ctx, client.ContainerListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Items))
	for _, c := range res.Items {
		out = append(out, c.ID)
	}
	return out, nil
}

// ContainerByName returns the id of the running container with exactly this name and its labels; ok is
// false when no container with this name runs. Docker's name filter matches substrings, so the names are
// compared here: the gateway must never exec into "edge_sim_docker-10" because "edge_sim_docker-1" was
// configured (R3, the simulated edge device).
func (r *Runtime) ContainerByName(ctx context.Context, name string) (id string, labels map[string]string, ok bool, err error) {
	if name == "" {
		return "", nil, false, nil
	}
	want := "/" + name
	f := make(client.Filters).Add("name", "^"+want+"$")
	res, err := r.cli.ContainerList(ctx, client.ContainerListOptions{Filters: f})
	if err != nil {
		return "", nil, false, err
	}
	for _, c := range res.Items {
		if !slices.Contains(c.Names, want) {
			continue
		}
		return c.ID, c.Labels, true, nil
	}
	return "", nil, false, nil
}

// TailBuffer keeps the last n bytes (pi's stderr, for error messages).
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

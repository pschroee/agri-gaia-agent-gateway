package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agw/internal/execproto"
)

// serve is the long-lived supervisor in the execution sandbox. The
// orchestrator starts it once per slot via docker exec as root (without
// capabilities except SETUID/SETGID) and sends requests over stdin. Each
// request is run by its own child process "agw-exec op" as the agent user.
// The agent can therefore neither end the supervisor nor write to its output
// via /proc (different user); at most it reaches the child process of its
// own operation.
type server struct {
	self string // path to agw-exec
	uid  int    // agent user; -1: do not switch (tests)
	gid  int
	env  []string

	outMu sync.Mutex
	out   *bufio.Writer

	mu      sync.Mutex
	running map[uint64]*child
	wg      sync.WaitGroup

	reapMu sync.Mutex
	// cgroup: directory with pids.max and pids.current (tests set it differently).
	cgroup string
	// bgMax: at most this many concurrent background tasks (operation bg).
	bgMax int
}

const maxConcurrentOps = 64

func newServer(out io.Writer, self string, uid, gid int) *server {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "USER", "LOGNAME", "MAIL", "SUDO_USER":
			continue
		}
		env = append(env, kv)
	}
	if uid >= 0 {
		env = append(env, "HOME=/home/agent", "USER=agent", "LOGNAME=agent")
	} else if h, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+h)
	}
	return &server{self: self, uid: uid, gid: gid, env: env, out: bufio.NewWriterSize(out, 64<<10), running: map[uint64]*child{}, cgroup: "/sys/fs/cgroup", bgMax: execproto.DefaultBgMax}
}

func (s *server) send(f execproto.Frame) {
	b, err := json.Marshal(f)
	if err != nil {
		b, _ = json.Marshal(execproto.Frame{ID: f.ID, Done: true, Error: "response not encodable"})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
	_ = s.out.Flush()
}

// run reads requests until the end of stdin. Afterwards all running
// operations are aborted: without an orchestrator nothing should keep running.
func (s *server) run(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), execproto.MaxFrameBytes)
	for sc.Scan() {
		var req execproto.Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.Op == execproto.OpCancel || req.Op == execproto.OpInput {
			s.mu.Lock()
			c := s.running[req.ID]
			s.mu.Unlock()
			if c == nil {
				continue
			}
			if req.Op == execproto.OpCancel {
				c.cancel()
			} else {
				c.input(sc.Bytes())
			}
			continue
		}
		if err := req.Validate(); err != nil {
			s.send(execproto.Frame{ID: req.ID, Done: true, Error: err.Error(), Code: "EINVAL"})
			continue
		}
		s.mu.Lock()
		busy := len(s.running) >= maxConcurrentOps
		_, dup := s.running[req.ID]
		bgFull := req.Op == execproto.OpBg && s.bgRunning() >= s.bgMax
		s.mu.Unlock()
		if busy || dup {
			s.send(execproto.Frame{ID: req.ID, Done: true, Error: "too many concurrent operations", Code: "EBUSY"})
			continue
		}
		if bgFull {
			s.send(execproto.Frame{ID: req.ID, Done: true, Error: fmt.Sprintf("too many background tasks (limit %d)", s.bgMax), Code: "ELIMIT"})
			continue
		}
		s.start(req)
	}
	s.mu.Lock()
	for _, c := range s.running {
		c.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// child is a running operation. Further input (workflow only) is written by its own
// writer, so that a child that does not read cannot hold up the supervisor; if its
// queue fills up, the operation is aborted.
type child struct {
	stdin io.WriteCloser
	once  sync.Once
	in    chan []byte
	done  chan struct{}
	bg    bool // background task (counts against bgMax)
	pgid  int  // process group of a background task (from its first frame)
	ended bool // final frame sent (no longer counts against bgMax)
}

// bgRunning counts the running background tasks; s.mu is locked.
func (s *server) bgRunning() int {
	n := 0
	for _, c := range s.running {
		if c.bg && !c.ended {
			n++
		}
	}
	return n
}

func newChild(stdin io.WriteCloser, withInput bool) *child {
	c := &child{stdin: stdin, done: make(chan struct{})}
	if withInput {
		c.in = make(chan []byte, 1024)
		go func() {
			for {
				select {
				case b := <-c.in:
					if _, err := c.stdin.Write(b); err != nil {
						c.cancel()
						return
					}
				case <-c.done:
					return
				}
			}
		}()
	}
	return c
}

func (c *child) cancel() {
	c.once.Do(func() {
		close(c.done)
		_ = c.stdin.Close()
	})
}

func (c *child) input(line []byte) {
	if c.in == nil {
		return
	}
	b := append(append([]byte(nil), line...), '\n')
	select {
	case c.in <- b:
	case <-c.done:
	default:
		c.cancel() // child does not read: abort instead of buffering without limit
	}
}

func (s *server) start(req execproto.Request) {
	id := req.ID
	// The supervisor creates the output file of a background task and hands it over open: the
	// agent cannot create anything in the directory beforehand or modify the file (Review 3, N1).
	req.LogFD = false
	var logFile *os.File
	if req.Op == execproto.OpBg {
		f, err := openBgLog(bgLogFile(req.Spill))
		if err != nil {
			fmt.Fprintln(os.Stderr, "agw-exec serve: output file of the background task:", err)
		} else {
			logFile, req.LogFD = f, true
		}
	}
	cmd, stdin, stdout, err := s.spawn(logFile)
	if err != nil && errors.Is(err, syscall.EAGAIN) {
		// No process left (PidsLimit exhausted, e.g. by a fork bomb): emergency brake,
		// then a second attempt (N3).
		s.reap()
		cmd, stdin, stdout, err = s.spawn(logFile)
	}
	if logFile != nil {
		logFile.Close() // the helper has its own copy
	}
	if err != nil {
		s.send(execproto.Frame{ID: id, Done: true, Error: "execution helper not started: " + err.Error(), Code: "EAGAIN"})
		return
	}
	b, _ := json.Marshal(req)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		s.send(execproto.Frame{ID: id, Done: true, Error: "execution helper does not accept the request", Code: "EIO"})
		return
	}
	c := newChild(stdin, req.Op == execproto.OpWorkflow)
	c.bg = req.Op == execproto.OpBg
	s.mu.Lock()
	s.running[id] = c
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		done := false
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), execproto.MaxFrameBytes)
		for sc.Scan() {
			var f execproto.Frame
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			f.ID = id
			if c.bg && (f.Pgid > 0 || f.Done) {
				s.mu.Lock()
				if f.Done {
					c.ended = true // before sending: whoever sees the end finds the slot free
				} else {
					c.pgid = f.Pgid
				}
				s.mu.Unlock()
			}
			s.send(f)
			if f.Done {
				done = true
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
		c.cancel()
		_ = cmd.Wait()
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
		if !done && c.bg {
			// The helper of a background task ended without a result (e.g. killed by the agent):
			// its process group should not keep running unobserved. The supervisor may do
			// this with CAP_KILL for the agent's processes too.
			s.mu.Lock()
			pg := c.pgid
			s.mu.Unlock()
			if pg > 1 {
				_ = syscall.Kill(-pg, syscall.SIGKILL)
			}
		}
		if !done {
			s.mu.Lock()
			c.ended = true
			s.mu.Unlock()
			msg := "execution helper exited without result"
			// Without free processes the helper's runtime already dies (Go needs threads).
			if s.pidsExhausted() {
				s.reap()
				msg += "; the process limit of the execution sandbox was exhausted, all processes of the agent were killed"
			}
			s.send(execproto.Frame{ID: id, Done: true, Error: msg, Code: "EIO"})
		}
	}()
}

// spawn starts a child process "agw-exec op" as the agent user; extra becomes descriptor 3 there.
func (s *server) spawn(extra *os.File) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	cmd := exec.Command(s.self, "op")
	cmd.Env = s.env
	cmd.Dir = "/"
	if extra != nil {
		cmd.ExtraFiles = []*os.File{extra}
	}
	attr := &syscall.SysProcAttr{Setpgid: true}
	if s.uid >= 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid), Groups: []uint32{}}
	}
	cmd.SysProcAttr = attr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}

// pidsExhausted: is the sandbox's PidsLimit (cgroup v2) almost reached?
func (s *server) pidsExhausted() bool {
	read := func(name string) (int64, bool) {
		b, err := os.ReadFile(filepath.Join(s.cgroup, name))
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return v, err == nil
	}
	max, ok1 := read("pids.max")
	cur, ok2 := read("pids.current")
	return ok1 && ok2 && cur >= max-4
}

// reap is the emergency brake when the PidsLimit is exhausted (security review N3): the supervisor
// (root with CAP_KILL, without needing new processes) kills all processes of the agent
// except PID 1, including those that escaped their process group. Running operations end
// with an error. Several rounds, because a loop of the agent keeps spawning until it ends.
func (s *server) reap() {
	if s.uid < 0 {
		return
	}
	s.reapMu.Lock()
	defer s.reapMu.Unlock()
	total := 0
	for round := 0; round < 20; round++ {
		n := killUID(s.uid)
		total += n
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "agw-exec serve: process limit exhausted, killed %d processes of the agent\n", total)
}

// killUID sends SIGKILL to all processes with the real uid (except PID 1 and itself).
func killUID(uid int) int {
	ents, _ := os.ReadDir("/proc")
	self := os.Getpid()
	n := 0
	want := strconv.Itoa(uid)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 || pid == self {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if f, ok := strings.CutPrefix(line, "Uid:"); ok {
				if fs := strings.Fields(f); len(fs) > 0 && fs[0] == want {
					if syscall.Kill(pid, syscall.SIGKILL) == nil {
						n++
					}
				}
				break
			}
		}
	}
	return n
}

// opMain is the child process: one request from the first line of stdin;
// the end of stdin aborts the operation.
func opMain() int {
	if err := notDumpable(); err != nil {
		return 2
	}
	r := bufio.NewReaderSize(os.Stdin, 64<<10)
	line, err := readLine(r, execproto.MaxFrameBytes)
	if err != nil {
		return 2
	}
	var req execproto.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Further lines on stdin are input (workflow only); the end of stdin aborts.
	// An empty "input" frame ends the input without aborting.
	input := make(chan []byte, 64)
	go func() {
		closed := false
		defer func() {
			if !closed {
				close(input)
			}
		}()
		for {
			line, err := readLine(r, execproto.MaxFrameBytes)
			if err != nil {
				cancel()
				return
			}
			if req.Op != execproto.OpWorkflow || closed {
				continue
			}
			var in execproto.Request
			if json.Unmarshal(line, &in) != nil || in.Op != execproto.OpInput {
				continue
			}
			if len(in.Data) == 0 {
				close(input)
				closed = true
				continue
			}
			select {
			case input <- in.Data:
			case <-ctx.Done():
				return
			}
		}
	}()
	if req.Op != execproto.OpBash && req.Op != execproto.OpWorkflow {
		// File operations and searches have a fixed upper limit.
		go func() {
			time.Sleep(10 * time.Minute)
			cancel()
			time.Sleep(5 * time.Second)
			os.Exit(3)
		}()
	}
	w := bufio.NewWriterSize(os.Stdout, 64<<10)
	var mu sync.Mutex
	emit := func(f execproto.Frame) {
		b, _ := json.Marshal(f)
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(append(b, '\n'))
		_ = w.Flush()
	}
	if req.Op == execproto.OpWorkflow {
		runWorkflow(ctx, req, input, emit)
	} else {
		runOp(ctx, req, emit)
	}
	return 0
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, io.ErrShortBuffer
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// ensureBgDir creates the directory of the output files as the supervisor (as root: owned by root,
// 0755). Whatever is already there and is not exactly that (symlink, file, directory of another
// user, other permissions), it moves aside.
func ensureBgDir(dir string) error {
	st, err := os.Lstat(dir)
	if err == nil {
		sys, _ := st.Sys().(*syscall.Stat_t)
		if st.IsDir() && sys != nil && int(sys.Uid) == os.Geteuid() {
			if st.Mode().Perm() != 0o755 {
				return os.Chmod(dir, 0o755)
			}
			return nil
		}
		aside := fmt.Sprintf("%s.foreign-%d", dir, time.Now().UnixNano())
		if err := os.Rename(dir, aside); err != nil {
			return fmt.Errorf("could not move foreign %s aside: %w", dir, err)
		}
		fmt.Fprintf(os.Stderr, "agw-exec serve: moved pre-existing %s to %s\n", dir, aside)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755) // independent of the umask
}

// openBgLog creates the output file of a background task (directory see ensureBgDir) and
// opens it for writing; a FIFO or a symlink at that path is replaced.
func openBgLog(p string) (*os.File, error) {
	if err := ensureBgDir(filepath.Dir(p)); err != nil {
		return nil, err
	}
	f, err := openSpillFile(p)
	if err == nil {
		return f, nil
	}
	// FIFO (ENXIO), symlink (ELOOP) or other: remove and create anew, without taking anything existing.
	_ = os.Remove(p)
	f, err = os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, err
	}
	if err := regularBlocking(f, p); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

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

// serve ist der langlebige Überwacher in der Ausführungs-Sandbox. Der
// Orchestrator startet ihn einmal je Platz per docker exec als root (ohne
// Capabilities außer SETUID/SETGID) und schickt Anfragen über stdin. Jede
// Anfrage führt ein eigener Kindprozess "agw-exec op" als Agent-Nutzer aus.
// Der Agent kann den Überwacher deshalb weder beenden noch über /proc seine
// Ausgabe beschreiben (anderer Nutzer); er erreicht höchstens den Kindprozess
// seiner eigenen Operation.
type server struct {
	self string // Pfad zu agw-exec
	uid  int    // Agent-Nutzer; -1: nicht wechseln (Tests)
	gid  int
	env  []string

	outMu sync.Mutex
	out   *bufio.Writer

	mu      sync.Mutex
	running map[uint64]*child
	wg      sync.WaitGroup

	reapMu sync.Mutex
	// cgroup: Verzeichnis mit pids.max und pids.current (Tests setzen es anders).
	cgroup string
	// bgMax: höchstens so viele Hintergrundaufgaben (Operation bg) gleichzeitig.
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
		b, _ = json.Marshal(execproto.Frame{ID: f.ID, Done: true, Error: "Antwort nicht kodierbar"})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
	_ = s.out.Flush()
}

// run liest Anfragen bis zum Ende von stdin. Danach werden alle laufenden
// Operationen abgebrochen: Ohne Orchestrator soll nichts weiterlaufen.
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

// child ist eine laufende Operation. Weitere Eingaben (nur workflow) schreibt ein eigener
// Schreiber, damit ein Kind, das nicht liest, den Überwacher nicht aufhält; läuft seine
// Warteschlange voll, wird die Operation abgebrochen.
type child struct {
	stdin io.WriteCloser
	once  sync.Once
	in    chan []byte
	done  chan struct{}
	bg    bool // Hintergrundaufgabe (zählt gegen bgMax)
	pgid  int  // Prozessgruppe einer Hintergrundaufgabe (aus ihrem ersten Rahmen)
	ended bool // abschließender Rahmen gesendet (zählt nicht mehr gegen bgMax)
}

// bgRunning zählt die laufenden Hintergrundaufgaben; s.mu ist gesperrt.
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
		c.cancel() // Kind liest nicht: abbrechen statt unbegrenzt zu puffern
	}
}

func (s *server) start(req execproto.Request) {
	id := req.ID
	// Die Ausgabedatei einer Hintergrundaufgabe legt der Überwacher an und übergibt sie offen: Der
	// Agent kann im Verzeichnis nichts vorab anlegen und die Datei nicht verändern (Review 3, N1).
	req.LogFD = false
	var logFile *os.File
	if req.Op == execproto.OpBg {
		f, err := openBgLog(bgLogFile(req.Spill))
		if err != nil {
			fmt.Fprintln(os.Stderr, "agw-exec serve: Ausgabedatei der Hintergrundaufgabe:", err)
		} else {
			logFile, req.LogFD = f, true
		}
	}
	cmd, stdin, stdout, err := s.spawn(logFile)
	if err != nil && errors.Is(err, syscall.EAGAIN) {
		// Kein Prozess mehr frei (PidsLimit erschöpft, etwa durch eine Fork-Bombe): Notbremse,
		// dann ein zweiter Versuch (N3).
		s.reap()
		cmd, stdin, stdout, err = s.spawn(logFile)
	}
	if logFile != nil {
		logFile.Close() // der Helfer hat seine Kopie
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
					c.ended = true // vor dem Senden: Wer das Ende sieht, findet den Platz frei
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
			// Der Helfer einer Hintergrundaufgabe ist ohne Ergebnis geendet (etwa vom Agenten
			// beendet): Ihre Prozessgruppe soll nicht unbeobachtet weiterlaufen. Der Überwacher
			// darf das mit CAP_KILL auch für Prozesse des Agenten.
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
			// Ohne freie Prozesse stirbt schon die Laufzeit des Helfers (Go braucht Threads).
			if s.pidsExhausted() {
				s.reap()
				msg += "; the process limit of the execution sandbox was exhausted, all processes of the agent were killed"
			}
			s.send(execproto.Frame{ID: id, Done: true, Error: msg, Code: "EIO"})
		}
	}()
}

// spawn startet einen Kindprozess „agw-exec op“ als Agent-Nutzer; extra wird dort Deskriptor 3.
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

// pidsExhausted: Ist das PidsLimit der Sandbox (cgroup v2) fast erreicht?
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

// reap ist die Notbremse bei erschöpftem PidsLimit (Security-Review N3): Der Überwacher
// (root mit CAP_KILL, ohne neue Prozesse zu brauchen) beendet alle Prozesse des Agenten
// außer PID 1, auch solche, die ihrer Prozessgruppe entkommen sind. Laufende Operationen enden
// dabei mit Fehler. Mehrere Runden, weil eine Schleife des Agenten bis zu ihrem Ende nachlegt.
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
	fmt.Fprintf(os.Stderr, "agw-exec serve: Prozesslimit erschöpft, %d Prozesse des Agenten beendet\n", total)
}

// killUID schickt SIGKILL an alle Prozesse mit realer uid (außer PID 1 und sich selbst).
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

// opMain ist der Kindprozess: eine Anfrage aus der ersten Zeile von stdin;
// das Ende von stdin bricht die Operation ab.
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
	// Weitere Zeilen auf stdin sind Eingaben (nur workflow); das Ende von stdin bricht ab.
	// Ein leerer Rahmen „input“ beendet die Eingaben, ohne abzubrechen.
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
		// Dateioperationen und Suchen haben eine feste Obergrenze.
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

// ensureBgDir legt das Verzeichnis der Ausgabedateien als Überwacher an (als root: root-eigen,
// 0755). Was dort schon liegt und nicht genau das ist (Verweis, Datei, Verzeichnis eines anderen
// Nutzers, andere Rechte), räumt er beiseite.
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
		aside := fmt.Sprintf("%s.fremd-%d", dir, time.Now().UnixNano())
		if err := os.Rename(dir, aside); err != nil {
			return fmt.Errorf("fremdes %s nicht beiseitegeräumt: %w", dir, err)
		}
		fmt.Fprintf(os.Stderr, "agw-exec serve: vorgefundenes %s nach %s verschoben\n", dir, aside)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755) // unabhängig von der umask
}

// openBgLog legt die Ausgabedatei einer Hintergrundaufgabe an (Verzeichnis siehe ensureBgDir) und
// öffnet sie zum Schreiben; eine FIFO oder ein Verweis an der Adresse wird ersetzt.
func openBgLog(p string) (*os.File, error) {
	if err := ensureBgDir(filepath.Dir(p)); err != nil {
		return nil, err
	}
	f, err := openSpillFile(p)
	if err == nil {
		return f, nil
	}
	// FIFO (ENXIO), Verweis (ELOOP) oder anderes: entfernen und neu anlegen, ohne Vorhandenes zu nehmen.
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

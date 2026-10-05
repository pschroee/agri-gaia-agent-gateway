package execbox

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/execproto"
)

// The test starts the real supervisor locally (without Docker and without
// switching users) and talks to it through the client.

var bin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "execbox")
	bin = filepath.Join(dir, "agw-exec")
	if out, err := exec.Command("go", "build", "-o", bin, "agw/cmd/agw-exec").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type localDialer struct {
	mu    sync.Mutex
	procs []*exec.Cmd
	dials int
}

func (d *localDialer) dial(ctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
	cmd := exec.Command(bin, "serve")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	d.mu.Lock()
	d.procs = append(d.procs, cmd)
	d.dials++
	d.mu.Unlock()
	var once sync.Once
	return in, out, func() { once.Do(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }) }, nil
}

func TestRunConcurrentStreamAndCancel(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash missing")
	}
	dir := t.TempDir()
	d := &localDialer{}
	c := New(d.dial)
	defer c.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := filepath.Join(dir, "f"+string(rune('a'+i)))
			if f, err := c.Run(ctx, execproto.Request{Op: execproto.OpWrite, Path: p, Data: []byte(p)}, nil); err != nil || f.Error != "" {
				t.Errorf("write: %v %+v", err, f)
				return
			}
			f, err := c.Run(ctx, execproto.Request{Op: execproto.OpRead, Path: p}, nil)
			var r execproto.ReadResult
			_ = json.Unmarshal(f.Result, &r)
			if err != nil || string(r.Data) != p {
				t.Errorf("read: %v %q", err, r.Data)
			}
		}(i)
	}
	wg.Wait()
	var out strings.Builder
	f, err := c.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: "for i in 1 2 3; do echo $i; done", Cwd: dir}, func(b []byte) { out.Write(b) })
	if err != nil || f.Exit == nil || *f.Exit != 0 || out.String() != "1\n2\n3\n" {
		t.Fatalf("bash: %v %+v %q", err, f, out.String())
	}
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	f, err = c.Run(cctx, execproto.Request{Op: execproto.OpBash, Command: "sleep 30", Cwd: dir}, nil)
	if err == nil || f.Code != "aborted" || time.Since(start) > 5*time.Second {
		t.Fatalf("abort: %v %+v after %v", err, f, time.Since(start))
	}
	if d.dials != 1 {
		t.Fatalf("%d connections instead of one", d.dials)
	}
}

// If the supervisor dies, running calls end with ErrLost, the next one
// restarts it.
func TestReconnectAfterLoss(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash missing")
	}
	dir := t.TempDir()
	d := &localDialer{}
	c := New(d.dial)
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), execproto.Request{Op: execproto.OpBash, Command: "sleep 30", Cwd: dir}, nil)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	d.mu.Lock()
	_ = d.procs[0].Process.Kill()
	d.mu.Unlock()
	select {
	case err := <-done:
		if err != ErrLost {
			t.Fatalf("expected ErrLost, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call does not end although the supervisor is gone")
	}
	f, err := c.Run(context.Background(), execproto.Request{Op: execproto.OpStat, Path: dir}, nil)
	if err != nil || f.Error != "" || d.dials != 2 {
		t.Fatalf("restart: %v %+v (%d connections)", err, f, d.dials)
	}
	c.Close()
	if _, err := c.Run(context.Background(), execproto.Request{Op: execproto.OpStat, Path: dir}, nil); err != ErrClosed {
		t.Fatalf("after Close: %v", err)
	}
	if _, err := c.Run(context.Background(), execproto.Request{Op: "read", Path: "relative"}, nil); err == nil {
		t.Fatal("invalid request accepted")
	}
}

// scriptedDialer is a supervisor in the test process: bash delivers n data frames and then Done,
// all other operations Done immediately.
func scriptedDialer(n int) Dialer {
	return func(ctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() {
			dec := json.NewDecoder(inR)
			enc := json.NewEncoder(outW)
			for {
				var req execproto.Request
				if dec.Decode(&req) != nil {
					outW.Close()
					return
				}
				if req.Op == execproto.OpBash {
					for i := 0; i < n; i++ {
						_ = enc.Encode(execproto.Frame{ID: req.ID, Data: []byte("x")})
					}
					code := 0
					_ = enc.Encode(execproto.Frame{ID: req.ID, Done: true, Exit: &code})
				} else if req.Op != execproto.OpCancel {
					_ = enc.Encode(execproto.Frame{ID: req.ID, Done: true, Result: json.RawMessage(`{}`)})
				}
			}
		}()
		return inW, outR, func() { inW.Close(); outR.Close() }, nil
	}
}

// M4: a slow reader (pi does not read a command's output) neither holds up the other
// operations of the slot, nor is its Done frame lost.
func TestSlowReaderDoesNotBlockOthers(t *testing.T) {
	c := New(scriptedDialer(2000))
	defer c.Close()
	ctx := context.Background()
	release := make(chan struct{})
	var got int
	bashDone := make(chan execproto.Frame, 1)
	go func() {
		f, _ := c.Run(ctx, execproto.Request{Op: execproto.OpBash, Command: "yes", Cwd: "/"}, func(b []byte) {
			if got == 0 {
				<-release
			}
			got += len(b)
		})
		bashDone <- f
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if f, err := c.Run(sctx, execproto.Request{Op: execproto.OpStat, Path: "/"}, nil); err != nil || !f.Done {
		t.Fatalf("stat behind slow reader: %+v %v after %v", f, err, time.Since(start))
	}
	close(release)
	select {
	case f := <-bashDone:
		if f.Exit == nil || *f.Exit != 0 || got != 2000 {
			t.Fatalf("bash: %+v, %d bytes", f, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Done frame of the slow reader missing")
	}
}

// M4: above the buffer limit the client discards data, aborts the operation and reports it
// in the Done frame, instead of taking unlimited memory.
func TestSlowReaderOverflow(t *testing.T) {
	old := maxQueuedBytes
	maxQueuedBytes = 100
	defer func() { maxQueuedBytes = old }()
	c := New(scriptedDialer(2000))
	defer c.Close()
	release := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(release) })
	first := true
	f, err := c.Run(context.Background(), execproto.Request{Op: execproto.OpBash, Command: "yes", Cwd: "/"}, func(b []byte) {
		if first {
			first = false
			<-release
		}
	})
	if err != nil || f.Code != "EOVERFLOW" || !strings.Contains(f.Error, "overflow") {
		t.Fatalf("overflow: %+v %v", f, err)
	}
}

// Background task through the client: onStart reports the process group before the output.
func TestRunBackground(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash missing")
	}
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_BG_DIR", dir)
	d := &localDialer{}
	c := New(d.dial)
	defer c.Close()
	var events []string
	f, err := c.RunBackground(context.Background(), execproto.Request{Op: execproto.OpBg, Command: "echo one; exit 4", Cwd: dir, Spill: execproto.BgLogPath(3)},
		func(pgid int) { events = append(events, "start") }, func(b []byte) { events = append(events, strings.TrimSpace(string(b))) })
	if err != nil || f.Exit == nil || *f.Exit != 4 {
		t.Fatalf("end: %+v %v", f, err)
	}
	if strings.Join(events, ",") != "start,one" {
		t.Fatalf("order: %v", events)
	}
}

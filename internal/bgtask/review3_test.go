package bgtask

// Tests for Review 3: ended tasks do not stay in the registry (M2), the limit also holds for
// concurrent starts (N2), reading along does not copy on every chunk (N3).

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/execproto"
	"agw/internal/store"
)

// M2: after many ended tasks the registry keeps only the last KeepEnded; Output falls back to the
// database for older ones.
func TestEndedTasksReleased(t *testing.T) {
	r, run, n := newReg(t, 5)
	n.endedCh = nil
	ctx := context.Background()
	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	const total = 400
	code := 0
	for i := 1; i <= total; i++ {
		cmd := fmt.Sprintf("big %d", i)
		if _, err := r.Start(ctx, StartParams{ChatID: "c1", Command: cmd, Cwd: "/workspace"}); err != nil {
			t.Fatal(err)
		}
		run.end(cmd, execproto.Frame{Done: true, Exit: &code})
		for deadline := time.Now().Add(2 * time.Second); r.Running() > 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	kept := len(r.tasks)
	r.mu.Unlock()
	grown := int64(heap()) - int64(before)
	t.Logf("%d ended tasks with 120 KiB each: %d in the registry, heap %+.1f MiB", total, kept, float64(grown)/(1<<20))
	if kept > KeepEnded {
		t.Fatalf("registry keeps %d ended tasks (at most %d)", kept, KeepEnded)
	}
	if grown > 16<<20 {
		t.Fatalf("heap grew by %.1f MiB", float64(grown)/(1<<20))
	}
	// The newest is still in the registry (full tail), the oldest comes from the database.
	if s, full, err := r.Output(ctx, "c1", store.BgID(total)); err != nil || s.State != store.BgExited || len(full) != TailBytes {
		t.Fatalf("newest: %+v %d %v", s.State, len(full), err)
	}
	if s, full, err := r.Output(ctx, "c1", "bg-1"); err != nil || s.State != store.BgExited || full != s.Tail || len(full) == 0 {
		t.Fatalf("oldest from the database: %+v %d %v", s.State, len(full), err)
	}
	if s, err := r.Stop(ctx, "c1", "bg-2", "user"); err != nil || s.State != store.BgExited {
		t.Fatalf("Stop of a released one: %+v %v", s, err)
	}
	if l := r.List("c1"); len(l) > KeepEnded {
		t.Fatalf("list: %d", len(l))
	}
}

// N2: 32 concurrent starts with limit 5: exactly five run, the rest is refused.
func TestStartLimitAtomic(t *testing.T) {
	r, _, n := newReg(t, 5)
	n.delay = 5 * time.Millisecond // creating the row in the database takes time
	var ok, limited atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := r.Start(context.Background(), StartParams{ChatID: "c1", Command: fmt.Sprintf("sleep %d", i), Cwd: "/workspace"})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrLimit):
				limited.Add(1)
			default:
				t.Errorf("Start %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if ok.Load() != 5 || limited.Load() != 27 || r.Running() != 5 {
		t.Fatalf("started %d, refused %d, running %d", ok.Load(), limited.Load(), r.Running())
	}
}

// N3: reading along a chunk allocates nothing new in steady state (ring buffer instead of append and
// copy of the whole tail per chunk).
func TestWriteNoAllocations(t *testing.T) {
	old := ProgressEvery
	ProgressEvery = time.Hour
	defer func() { ProgressEvery = old }()
	tk := &task{t: store.BackgroundTask{ChatID: "c", Seq: 1}, done: make(chan struct{}), sum: newHash()}
	defer close(tk.done)
	w := tk.write(&fakeNotifier{})
	chunk := []byte(strings.Repeat("0123456789abcde\n", 2<<10)) // 32 KiB
	for i := 0; i < 8; i++ {
		w(chunk) // warm-up: buffer, head, timer
	}
	if a := testing.AllocsPerRun(200, func() { w(chunk) }); a > 0 {
		t.Fatalf("%.1f allocations per chunk", a)
	}
	s := tk.snapshot()
	if !strings.HasSuffix(s.Tail, "0123456789abcde\n") || len(s.Tail) != ShortTailBytes || s.OutputLines == 0 || s.OutputLines%(2<<10) != 0 {
		t.Fatalf("state: %d bytes tail, %d lines", len(s.Tail), s.OutputLines)
	}
	tk.mu.Lock()
	full := tk.tail.bytes()
	tk.mu.Unlock()
	if len(full) != TailBytes || string(full[len(full)-16:]) != "0123456789abcde\n" || string(full[:16]) != "0123456789abcde\n" {
		t.Fatalf("tail in the ring buffer: %d %q", len(full), full[:16])
	}
}

func TestRing(t *testing.T) {
	var r ring
	r.init(10)
	r.write([]byte("abc"))
	if string(r.bytes()) != "abc" || string(r.last(2)) != "bc" {
		t.Fatalf("%q", r.bytes())
	}
	r.write([]byte("defghij"))
	r.write([]byte("klm"))
	if string(r.bytes()) != "defghijklm" || string(r.last(4)) != "jklm" || string(r.last(99)) != "defghijklm" {
		t.Fatalf("%q %q", r.bytes(), r.last(4))
	}
	r.write([]byte("0123456789ABCDEF"))
	if string(r.bytes()) != "6789ABCDEF" {
		t.Fatalf("%q", r.bytes())
	}
}

// cost per 32 KiB chunk (go test -bench Write ./internal/bgtask/).
func BenchmarkWrite(b *testing.B) {
	old := ProgressEvery
	ProgressEvery = time.Hour
	defer func() { ProgressEvery = old }()
	tk := &task{t: store.BackgroundTask{ChatID: "c", Seq: 1}, done: make(chan struct{}), sum: newHash()}
	defer close(tk.done)
	w := tk.write(&fakeNotifier{})
	chunk := []byte(strings.Repeat("0123456789abcde\n", 2<<10))
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w(chunk)
	}
}

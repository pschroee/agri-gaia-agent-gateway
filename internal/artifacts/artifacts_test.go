package artifacts

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrokerApprove(t *testing.T) {
	b := NewBroker()
	w := b.Register("a1")
	go func() {
		time.Sleep(20 * time.Millisecond)
		if !b.Resolve("a1", true) {
			t.Error("Resolve found no waiter")
		}
	}()
	approved, err := w.Wait(context.Background(), time.Second)
	if err != nil || !approved {
		t.Fatalf("expected approved: %v %v", approved, err)
	}
	if b.Resolve("a1", false) {
		t.Fatal("second Resolve took effect")
	}
}

func TestBrokerTimeoutIsRejection(t *testing.T) {
	b := NewBroker()
	w := b.Register("a2")
	approved, err := w.Wait(context.Background(), 30*time.Millisecond)
	if approved || err != ErrTimeout {
		t.Fatalf("expected timeout as rejection: %v %v", approved, err)
	}
	if b.Pending() != 0 {
		t.Fatal("waiter not removed after timeout")
	}
}

func TestBrokerCancelledContext(t *testing.T) {
	b := NewBroker()
	w := b.Register("a3")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if approved, err := w.Wait(ctx, time.Second); approved || err == nil {
		t.Fatalf("cancelled context: %v %v", approved, err)
	}
}

func TestBrokerResolveBeforeWait(t *testing.T) {
	b := NewBroker()
	w := b.Register("a4")
	b.Resolve("a4", true)
	if ok, err := w.Wait(context.Background(), time.Second); !ok || err != nil {
		t.Fatalf("decision before Wait got lost: %v %v", ok, err)
	}
}

func TestBrokerConcurrent(t *testing.T) {
	b := NewBroker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		id := string(rune('A' + i))
		w := b.Register(id)
		wg.Add(2)
		go func() { defer wg.Done(); w.Wait(context.Background(), time.Second) }()
		go func() { defer wg.Done(); b.Resolve(id, i%2 == 0) }()
	}
	wg.Wait()
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"report.csv":       "report.csv",
		"../../etc/passwd": "passwd",
		"a/b/c.txt":        "c.txt",
		"  empty  .txt ":   "empty  .txt",
		"":                 "",
		"..":               "",
		"Größe ä.txt":      "Größe ä.txt",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsText(t *testing.T) {
	if !IsText([]byte("a,b\n1,2\n")) || IsText([]byte{0x89, 'P', 'N', 'G', 0, 1}) {
		t.Fatal("IsText wrong")
	}
}

// Integration against RustFS: AGW_TEST_S3_ENDPOINT, RUSTFS_ACCESS_KEY, RUSTFS_SECRET_KEY.
func TestS3Integration(t *testing.T) {
	ep := os.Getenv("AGW_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("AGW_TEST_S3_ENDPOINT not set")
	}
	ctx := context.Background()
	s, err := NewS3(ctx, ep, os.Getenv("RUSTFS_ACCESS_KEY"), os.Getenv("RUSTFS_SECRET_KEY"), "agw-test-"+time.Now().Format("150405"))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello world")
	if err := s.Put(ctx, "pending/x/1", bytes.NewReader(data), int64(len(data)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(ctx, "pending/x/1", "x/output/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "pending/x/1"); err == nil {
		t.Fatal("source still present after Move")
	}
	r, size, err := s.Get(ctx, "x/output/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if size != int64(len(data)) || string(got) != "hello world" {
		t.Fatalf("read: %q (%d)", got, size)
	}
	if err := s.Delete(ctx, "x/output/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveBucket(ctx); err != nil && !strings.Contains(err.Error(), "not empty") {
		t.Fatal(err)
	}
}

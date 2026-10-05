// Package artifacts stores files in S3 (RustFS) and mediates the
// confirmation by the user: an upload waits until the user decides in the
// UI. This is enforced here in the orchestrator, not
// in pi.
package artifacts

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrTimeout = errors.New("no decision within the waiting time")

// Broker connects waiting uploads with the decisions from the API.
type Broker struct {
	mu      sync.Mutex
	waiters map[string]chan bool
}

func NewBroker() *Broker { return &Broker{waiters: map[string]chan bool{}} }

type Waiter struct {
	b  *Broker
	id string
	ch chan bool
}

func (b *Broker) Register(id string) *Waiter {
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.waiters[id] = ch
	b.mu.Unlock()
	return &Waiter{b: b, id: id, ch: ch}
}

// Resolve delivers the decision to the waiter. false if nobody is waiting.
func (b *Broker) Resolve(id string, approve bool) bool {
	b.mu.Lock()
	ch, ok := b.waiters[id]
	delete(b.waiters, id)
	b.mu.Unlock()
	if ok {
		ch <- approve
	}
	return ok
}

func (b *Broker) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}

// Wait blocks until the decision. A timeout counts as rejection.
func (w *Waiter) Wait(ctx context.Context, timeout time.Duration) (bool, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case v := <-w.ch:
		return v, nil
	case <-t.C:
		w.cancel()
		return false, ErrTimeout
	case <-ctx.Done():
		w.cancel()
		return false, ctx.Err()
	}
}

func (w *Waiter) cancel() {
	w.b.mu.Lock()
	if w.b.waiters[w.id] == w.ch {
		delete(w.b.waiters, w.id)
	}
	w.b.mu.Unlock()
}

// SanitizeName turns a name given by the agent into a flat file name.
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSpace(path.Base(strings.TrimSpace(name)))
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	if !utf8.ValidString(name) || len(name) > 200 {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
}

// IsText estimates whether the start of a file is text (for the preview).
func IsText(b []byte) bool {
	if len(b) > 4096 {
		b = b[:4096]
	}
	for len(b) > 0 && !utf8.Valid(b) && len(b) > 4088 {
		b = b[:len(b)-1] // truncated character at the end
	}
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// S3 is the store in RustFS (S3-compatible).
type S3 struct {
	c      *minio.Client
	bucket string
}

func NewS3(ctx context.Context, endpoint, access, secret, bucket string) (*S3, error) {
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: false})
	if err != nil {
		return nil, err
	}
	s := &S3{c: c, bucket: bucket}
	var lastErr error
	for i := 0; i < 30; i++ {
		ok, err := c.BucketExists(ctx, bucket)
		if err == nil && ok {
			return s, nil
		}
		if err == nil {
			if err = c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err == nil {
				return s, nil
			}
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, lastErr
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.c.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	o, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	st, err := o.Stat()
	if err != nil {
		o.Close()
		return nil, 0, err
	}
	return o, st.Size, nil
}

func (s *S3) Move(ctx context.Context, src, dst string) error {
	_, err := s.c.CopyObject(ctx, minio.CopyDestOptions{Bucket: s.bucket, Object: dst}, minio.CopySrcOptions{Bucket: s.bucket, Object: src})
	if err != nil {
		return err
	}
	return s.Delete(ctx, src)
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *S3) RemoveBucket(ctx context.Context) error { return s.c.RemoveBucket(ctx, s.bucket) }

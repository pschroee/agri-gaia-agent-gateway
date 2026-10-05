// Package execbox talks to the supervisor agw-exec serve in the
// execution sandbox of a slot (E9). One connection (one docker exec)
// carries any number of concurrent operations, distinguished by ID.
// If the supervisor dies, it is restarted on the next call.
package execbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"agw/internal/execproto"
)

// Dialer starts a supervisor and returns its stdin and stdout.
// close terminates it.
type Dialer func(ctx context.Context) (stdin io.WriteCloser, stdout io.Reader, close func(), err error)

type Client struct {
	dial   Dialer
	nextID atomic.Uint64

	mu     sync.Mutex
	sess   *session
	closed bool
}

func New(d Dialer) *Client { return &Client{dial: d} }

var (
	ErrClosed = errors.New("execution sandbox closed")
	ErrLost   = errors.New("connection to the execution sandbox lost")
)

type session struct {
	in    io.WriteCloser
	wmu   sync.Mutex
	close func()

	mu      sync.Mutex
	pending map[uint64]*queue
	done    chan struct{}
}

// maxQueuedBytes: this many data bytes the client buffers per operation when the caller
// reads more slowly than the sandbox delivers. Beyond that further data is discarded and the
// operation aborted; the Done frame is never discarded (code review M4).
var maxQueuedBytes = 64 << 20

// queue buffers the frames of an operation. The connection's reader stores them without ever
// blocking; so a slow caller does not hold up any other operation of the slot.
type queue struct {
	mu       sync.Mutex
	frames   []execproto.Frame
	bytes    int
	overflow bool
	notify   chan struct{} // capacity 1: "something is pending"
}

func newQueue() *queue { return &queue{notify: make(chan struct{}, 1)} }

// push stores a frame; true means the limit has just been exceeded.
func (q *queue) push(f execproto.Frame) (overflowNow bool) {
	q.mu.Lock()
	if !f.Done && len(f.Data) > 0 {
		if q.overflow || q.bytes+len(f.Data) > maxQueuedBytes {
			first := !q.overflow
			q.overflow = true
			q.mu.Unlock()
			return first
		}
		q.bytes += len(f.Data)
	}
	q.frames = append(q.frames, f)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return false
}

func (q *queue) pop() (execproto.Frame, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return execproto.Frame{}, false
	}
	f := q.frames[0]
	q.frames[0] = execproto.Frame{}
	q.frames = q.frames[1:]
	q.bytes -= len(f.Data)
	return f, true
}

func (c *Client) session(ctx context.Context) (*session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.sess != nil {
		select {
		case <-c.sess.done:
			c.sess = nil
		default:
			return c.sess, nil
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	in, out, closeFn, err := c.dial(dctx)
	if err != nil {
		return nil, err
	}
	s := &session{in: in, close: closeFn, pending: map[uint64]*queue{}, done: make(chan struct{})}
	go s.read(out)
	c.sess = s
	return s, nil
}

func (s *session) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), execproto.MaxFrameBytes+1<<20)
	for sc.Scan() {
		var f execproto.Frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		s.mu.Lock()
		q := s.pending[f.ID]
		s.mu.Unlock()
		if q == nil {
			continue // operation already finished or aborted
		}
		if q.push(f) {
			// The caller no longer reads: abort the operation instead of buffering without limit.
			// Do not send from the reader: writing can block while the other side is writing.
			go func(id uint64) { _ = s.send(execproto.Request{ID: id, Op: execproto.OpCancel}) }(f.ID)
		}
	}
	s.mu.Lock()
	close(s.done)
	s.mu.Unlock()
	s.close()
}

func (s *session) send(r execproto.Request) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.in.Write(append(b, '\n'))
	return err
}

// Run executes an operation. Data of a command goes to onData (may
// be nil). If ctx ends, Run aborts the operation in the sandbox and
// waits briefly for its end. The result is the last frame.
func (c *Client) Run(ctx context.Context, req execproto.Request, onData func([]byte)) (execproto.Frame, error) {
	return c.RunDuplex(ctx, req, nil, onData)
}

// RunDuplex is Run with further input to the running operation (workflow only): every line
// from input goes to the supervisor as an "input" frame with the same ID.
func (c *Client) RunDuplex(ctx context.Context, req execproto.Request, input <-chan []byte, onData func([]byte)) (execproto.Frame, error) {
	return c.run(ctx, req, input, onData, nil)
}

// RunBackground is Run for a background task (operation bg): onStart receives the
// process group from the first frame as soon as the command is running (not if it never starts).
func (c *Client) RunBackground(ctx context.Context, req execproto.Request, onStart func(pgid int), onData func([]byte)) (execproto.Frame, error) {
	return c.run(ctx, req, nil, onData, onStart)
}

func (c *Client) run(ctx context.Context, req execproto.Request, input <-chan []byte, onData func([]byte), onStart func(int)) (execproto.Frame, error) {
	if err := req.Validate(); err != nil {
		return execproto.Frame{}, err
	}
	s, err := c.session(ctx)
	if err != nil {
		return execproto.Frame{}, err
	}
	req.ID = c.nextID.Add(1)
	q := newQueue()
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return execproto.Frame{}, ErrLost
	default:
	}
	s.pending[req.ID] = q
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, req.ID)
		s.mu.Unlock()
	}()
	if err := s.send(req); err != nil {
		return execproto.Frame{}, ErrLost
	}
	if input != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case b, ok := <-input:
					if !ok {
						// End of input: an empty "input" frame (the operation then ends
						// normally; closing stdin would be an abort).
						_ = s.send(execproto.Request{ID: req.ID, Op: execproto.OpInput})
						return
					}
					if len(b) == 0 {
						continue
					}
					if s.send(execproto.Request{ID: req.ID, Op: execproto.OpInput, Data: b}) != nil {
						return
					}
				case <-stop:
					return
				}
			}
		}()
	}
	var cancelled <-chan time.Time
	ctxDone := ctx.Done()
	for {
		for {
			f, ok := q.pop()
			if !ok {
				break
			}
			if f.Pgid > 0 && !f.Done && onStart != nil {
				onStart(f.Pgid)
			}
			if len(f.Data) > 0 && onData != nil {
				onData(f.Data)
			}
			if f.Done {
				q.mu.Lock()
				over := q.overflow
				q.mu.Unlock()
				if over && f.Error == "" {
					f.Error, f.Code, f.Exit = "output overflow: reader too slow, output discarded after 64 MiB", "EOVERFLOW", nil
				}
				if cancelled != nil {
					return f, ctx.Err()
				}
				return f, nil
			}
		}
		select {
		case <-q.notify:
		case <-ctxDone:
			ctxDone = nil
			_ = s.send(execproto.Request{ID: req.ID, Op: execproto.OpCancel})
			cancelled = time.After(10 * time.Second)
		case <-cancelled:
			return execproto.Frame{ID: req.ID, Done: true, Error: "aborted", Code: "aborted"}, ctx.Err()
		case <-s.done:
			// Deliver still stored frames (including Done) first.
			q.mu.Lock()
			n := len(q.frames)
			q.mu.Unlock()
			if n > 0 {
				continue
			}
			return execproto.Frame{}, ErrLost
		}
	}
}

// Close terminates the supervisor; running operations end with ErrLost.
func (c *Client) Close() {
	c.mu.Lock()
	s := c.sess
	c.sess, c.closed = nil, true
	c.mu.Unlock()
	if s != nil {
		_ = s.in.Close()
		s.close()
	}
}

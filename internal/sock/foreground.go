package sock

// Running foreground commands (bash without run_in_background) of a slot. The user can stop one
// of them or convert it into a background task (like Ctrl+B in Claude Code); the agent
// learns about either from the result of the tool call.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"agw/internal/store"
)

// Control errors; the API turns them into 404 and 409 respectively.
var (
	ErrNoForeground = errors.New("no running command with this toolCallId")
	ErrNoBackground = errors.New("background tasks are not available on this slot")
)

// Foreground keeps track of the running foreground commands of a slot.
type Foreground struct {
	mu  sync.Mutex
	ops map[string]*fgOp // toolCallId
}

func NewForeground() *Foreground { return &Foreground{ops: map[string]*fgOp{}} }

type fgOp struct {
	chat    string
	cancel  context.CancelFunc
	stopped atomic.Bool
	detach  chan detachReq
	done    chan struct{} // the handler no longer accepts a conversion (command ended or converted)
}

type detachReq struct {
	ctx   context.Context
	reply chan detachResult
}

type detachResult struct {
	task store.BackgroundTask
	err  error
}

// add creates the control of a command. It is registered only if the ID is free:
// user_bash always comes with the same ID, and subagents can repeat IDs. A
// second command with the same ID then runs without stop and conversion, instead of taking
// control away from the first (code review 2026-09-30).
func (f *Foreground) add(id, chat string, cancel context.CancelFunc) *fgOp {
	op := &fgOp{chat: chat, cancel: cancel, detach: make(chan detachReq), done: make(chan struct{})}
	if id == "user_bash" {
		return op
	}
	f.mu.Lock()
	if _, taken := f.ops[id]; !taken {
		f.ops[id] = op
	}
	f.mu.Unlock()
	return op
}

func (f *Foreground) remove(id string, op *fgOp) {
	f.mu.Lock()
	if f.ops[id] == op {
		delete(f.ops, id)
	}
	f.mu.Unlock()
}

func (f *Foreground) get(chat, id string) (*fgOp, error) {
	f.mu.Lock()
	op := f.ops[id]
	f.mu.Unlock()
	if op == nil || op.chat != chat {
		return nil, ErrNoForeground
	}
	return op, nil
}

// Running lists the toolCallIds of the chat's running foreground commands.
func (f *Foreground) Running(chat string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for id, op := range f.ops {
		if op.chat == chat {
			out = append(out, id)
		}
	}
	return out
}

// Stop aborts the command; the agent gets "Command stopped by the user" along with the output so far.
func (f *Foreground) Stop(chat, toolCallID string) error {
	op, err := f.get(chat, toolCallID)
	if err != nil {
		return err
	}
	op.stopped.Store(true)
	op.cancel()
	return nil
}

// Background converts the command into a background task: it keeps running, the tool call
// ends immediately with a pointer to the task, and the orchestrator reports its end as for any
// background task.
func (f *Foreground) Background(ctx context.Context, chat, toolCallID string) (store.BackgroundTask, error) {
	op, err := f.get(chat, toolCallID)
	if err != nil {
		return store.BackgroundTask{}, err
	}
	req := detachReq{ctx: ctx, reply: make(chan detachResult, 1)}
	select {
	case op.detach <- req:
	case <-op.done:
		return store.BackgroundTask{}, ErrNoForeground
	case <-ctx.Done():
		return store.BackgroundTask{}, ctx.Err()
	}
	r := <-req.reply
	return r.task, r.err
}

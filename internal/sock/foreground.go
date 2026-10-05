package sock

// Laufende Vordergrundbefehle (bash ohne run_in_background) eines Platzes. Der Nutzer kann einen
// davon stoppen oder in eine Hintergrundaufgabe umwandeln (wie Strg+B in Claude Code); der Agent
// erfährt beides aus dem Ergebnis des Werkzeugaufrufs.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"agw/internal/store"
)

// Fehler der Steuerung; die API macht daraus 404 bzw. 409.
var (
	ErrNoForeground = errors.New("kein laufender Befehl mit dieser toolCallId")
	ErrNoBackground = errors.New("Hintergrundaufgaben sind auf diesem Platz nicht verfügbar")
)

// Foreground führt die laufenden Vordergrundbefehle eines Platzes.
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
	done    chan struct{} // der Handler nimmt keine Umwandlung mehr an (Befehl beendet oder umgewandelt)
}

type detachReq struct {
	ctx   context.Context
	reply chan detachResult
}

type detachResult struct {
	task store.BackgroundTask
	err  error
}

// add legt die Steuerung eines Befehls an. Eingetragen wird er nur, wenn die Kennung frei ist:
// user_bash kommt immer mit derselben Kennung, und Subagenten können Kennungen wiederholen. Ein
// zweiter Befehl mit derselben Kennung läuft dann ohne Stopp und Umwandlung, statt dem ersten die
// Steuerung zu nehmen (Code-Review 30.09.2026).
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

// Running nennt die toolCallIds der laufenden Vordergrundbefehle des Chats.
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

// Stop bricht den Befehl ab; der Agent bekommt „Command stopped by the user“ samt bisheriger Ausgabe.
func (f *Foreground) Stop(chat, toolCallID string) error {
	op, err := f.get(chat, toolCallID)
	if err != nil {
		return err
	}
	op.stopped.Store(true)
	op.cancel()
	return nil
}

// Background wandelt den Befehl in eine Hintergrundaufgabe um: Er läuft weiter, der Werkzeugaufruf
// endet sofort mit dem Hinweis auf die Aufgabe, und ihr Ende meldet der Orchestrator wie bei jeder
// Hintergrundaufgabe.
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

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// A-E2: POST /train/containers/<id>/run starts a task that ends when the container runs; the same job is
// then observed on as a training_run (the chain), because the task says nothing about the training.
func TestTrainingRunChain(t *testing.T) {
	req := platform.Request{Method: http.MethodPost, Path: "/train/containers/9/run"}
	res := platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/11"}
	kind, ref, ok := For(delegation.Access{}, req, res)
	if !ok || kind != kindPlatformTask || ref != "task:11|train:9" {
		t.Fatalf("For = %q %q %v", kind, ref, ok)
	}
	// The task runs first.
	read, calls := recorded(t, map[string]string{"GET /tasks/11": `{"status":"inprogress","completion_percentage":0.5}`})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: ref}, read)
	if err != nil || p.State != StateRunning || p.Next != nil {
		t.Fatalf("running task: %+v %v", p, err)
	}
	// The task is through: the row is handed over to the training run.
	read, _ = recorded(t, map[string]string{"GET /tasks/11": `{"status":"completed","completion_percentage":1.0}`})
	p, err = pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: ref}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.Next == nil || p.Next.Kind != kindTrainingRun || p.Next.Ref != "9" {
		t.Fatalf("hand-over: %+v", p)
	}
	if len(calls()) != 1 {
		t.Fatalf("calls: %v", calls())
	}
	// A task that failed is a failure; nothing is handed over.
	read, _ = recorded(t, map[string]string{"GET /tasks/11": `{"status":"failed","message":"cannot start"}`})
	if p, err = pollPlatformTask(context.Background(), Job{ID: "w1", Ref: ref}, read); err != nil || p.State != StateFailed || p.Next != nil {
		t.Fatalf("failed task: %+v %v", p, err)
	}
}

// A-E2: the state of a training container. Running and created mean running, exited with a score means
// done, an exited container whose log names an error fails, anything else stays running; the log tail
// comes along.
func TestTrainingRunStates(t *testing.T) {
	answers := func(status, score, logs string) map[string]string {
		return map[string]string{
			"GET /train/containers/9/status":       `{"status":"` + status + `","score":` + score + `}`,
			"GET /train/containers/9/logs?tail=20": `{"logs":` + logs + `}`,
		}
	}
	cases := []struct {
		name         string
		answers      map[string]string
		state, msg   string
		logInMessage string
	}{
		{"created", answers("created", "null", `""`), StateRunning, "created", ""},
		{"running", answers("running", "null", `"epoch 1\nepoch 2\n"`), StateRunning, "running", "epoch 2"},
		{"exited with a score", answers("exited", "0.97", `"done\n"`), StateDone, "exited, score 0.97", "done"},
		{"exited without an error", answers("exited", "null", `"finished\n"`), StateDone, "exited", "finished"},
		{"exited after an error", answers("exited", "null", `"Traceback (most recent call last)\nError: no data\n"`), StateFailed, "exited", "Error: no data"},
		{"dead", answers("dead", "null", `""`), StateFailed, "dead", ""},
	}
	for _, c := range cases {
		read, calls := recorded(t, c.answers)
		p, err := pollTrainingRun(context.Background(), Job{ID: "w1", Kind: kindTrainingRun, Ref: "9"}, read)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.State != c.state || p.Message != c.msg {
			t.Errorf("%s: %+v", c.name, p)
		}
		if p.Percent != nil {
			t.Errorf("%s: a training reports no percent: %v", c.name, *p.Percent)
		}
		if c.logInMessage != "" && !strings.Contains(p.LogTail, c.logInMessage) {
			t.Errorf("%s: log %q misses %q", c.name, p.LogTail, c.logInMessage)
		}
		if len(calls()) != 2 {
			t.Errorf("%s: calls %v", c.name, calls())
		}
	}
	// A container whose logs are not readable is still polled: the status decides.
	read, _ := recorded(t, map[string]string{"GET /train/containers/9/status": `{"status":"exited","score":0.5}`})
	p, err := pollTrainingRun(context.Background(), Job{ID: "w1", Ref: "9"}, read)
	if err != nil || p.State != StateDone {
		t.Fatalf("without logs: %+v %v", p, err)
	}
	// Without a reference there is nothing to ask.
	if _, err := pollTrainingRun(context.Background(), Job{ID: "w1", Ref: ""}, read); err == nil {
		t.Error("a job without a container must be an error")
	}
}

// A-E2 (D2.9): a running training can be stopped; the call belongs to the kind.
func TestTrainingRunStop(t *testing.T) {
	req, ok := stopTrainingRun(Job{ID: "w1", Kind: kindTrainingRun, Ref: "9"})
	if !ok || req.Method != http.MethodPost || req.Path != "/train/containers/9/stop" {
		t.Fatalf("stop: %+v %v", req, ok)
	}
	if _, ok := stopTrainingRun(Job{ID: "w1"}); ok {
		t.Error("a job without a container cannot be stopped")
	}
	// A platform task has no stop call: its route is the platform's own.
	k, ok := Lookup(kindPlatformTask)
	if !ok || k.Stop != nil {
		t.Fatalf("a platform task must not offer a stop: %+v", k)
	}
	// The training kind is registered with its poll and stop functions.
	k, ok = Lookup(kindTrainingRun)
	if !ok || k.Poll == nil || k.Stop == nil {
		t.Fatalf("training_run: %+v %v", k, ok)
	}
}

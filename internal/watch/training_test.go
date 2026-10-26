// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
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
			"GET /train/containers/9/status":        `{"status":"` + status + `","score":` + score + `}`,
			"GET /train/containers/9/logs?tail=100": `{"logs":` + logs + `}`,
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
		if len(calls()) != 3 { // status, logs, metrics (issue #168)
			t.Errorf("%s: calls %v", c.name, calls())
		}
	}
	{
		// A traceback above many redrawn progress bars is still found, and the stored tail keeps it (issue #178).
		bars, err := os.ReadFile("testdata/traceback-under-bars.log")
		if err != nil {
			t.Fatal(err)
		}
		enc, _ := json.Marshal(string(bars))
		read, _ := recorded(t, answers("exited", "null", string(enc)))
		p, err := pollTrainingRun(context.Background(), Job{ID: "w1", Kind: kindTrainingRun, Ref: "9"}, read)
		if err != nil || p.State != StateFailed {
			t.Fatalf("traceback under bars: %+v %v", p, err)
		}
		if !strings.Contains(p.LogTail, "RuntimeError") || strings.Count(p.LogTail, "Epoch ") > 1 || strings.Contains(p.LogTail, "\r") {
			t.Errorf("log tail: %q", p.LogTail)
		}
		// The real end of a clean test run: exited without a score and without an error counts as done.
		clean, err := os.ReadFile("testdata/lightning-test-tail.log")
		if err != nil {
			t.Fatal(err)
		}
		enc, _ = json.Marshal(string(clean))
		read, _ = recorded(t, answers("exited", "null", string(enc)))
		if p, err = pollTrainingRun(context.Background(), Job{ID: "w1", Kind: kindTrainingRun, Ref: "9"}, read); err != nil || p.State != StateDone {
			t.Fatalf("real tail: %+v %v", p, err)
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

// Issue #168: the figures of the run give the training a percent (epoch / epochs, at most 99 while it runs) and
// with it an ETA; the GPU samples come along.
func TestPollTrainingRunPercentFromEpochs(t *testing.T) {
	metrics := `{"supported":true,"gpu_available":true,"epochs":[` +
		`{"epoch":0,"epochs":10,"val_acc":0.4,"at":"2026-10-20T10:00:00Z"},` +
		`{"epoch":3,"epochs":10,"val_acc":0.6,"at":"2026-10-20T10:01:00Z"}],` +
		`"gpu":[{"at":"2026-10-20T10:00:05Z","util":80,"mem_used_mb":1000,"mem_total_mb":8000},` +
		`{"at":"2026-10-20T10:00:10Z","util":120,"mem_used_mb":1000,"mem_total_mb":8000}]}`
	read, _ := recorded(t, map[string]string{
		"GET /train/containers/9/status":        `{"status":"running","score":null}`,
		"GET /train/containers/9/logs?tail=100": `{"logs":""}`,
		"GET /train/containers/9/metrics":       metrics,
	})
	p, err := pollTrainingRun(context.Background(), Job{ID: "w1", Ref: "9"}, read)
	if err != nil || p.State != StateRunning {
		t.Fatalf("poll: %+v %v", p, err)
	}
	if p.Percent == nil || *p.Percent != 30 {
		t.Fatalf("percent: %v", p.Percent)
	}
	if p.Metrics == nil || !p.Metrics.Supported || !p.Metrics.GPUAvailable || len(p.Metrics.Epochs) != 2 {
		t.Fatalf("metrics: %+v", p.Metrics)
	}
	if len(p.Metrics.GPU) != 1 || p.Metrics.GPU[0].Util != 80 {
		t.Errorf("a sample above 100 percent falls away: %+v", p.Metrics.GPU)
	}
	// The last epoch while running never reaches 100.
	m := &Metrics{Epochs: nil}
	if MetricsPercent(m, false) != nil {
		t.Error("no epochs, no percent")
	}
	// A finished run is 100; a failed one is not.
	read, _ = recorded(t, map[string]string{
		"GET /train/containers/9/status":        `{"status":"exited","score":0.9}`,
		"GET /train/containers/9/logs?tail=100": `{"logs":""}`,
		"GET /train/containers/9/metrics":       metrics,
	})
	if p, _ = pollTrainingRun(context.Background(), Job{ID: "w1", Ref: "9"}, read); p.State != StateDone || p.Percent == nil || *p.Percent != 100 {
		t.Errorf("done: %+v", p)
	}
	// An older backend or an unreadable answer: no figures, no percent, the status decides.
	read, _ = recorded(t, map[string]string{
		"GET /train/containers/9/status":  `{"status":"running","score":null}`,
		"GET /train/containers/9/metrics": `{"supported":false,"epochs":[],"gpu":[],"gpu_available":false}`,
	})
	if p, _ = pollTrainingRun(context.Background(), Job{ID: "w1", Ref: "9"}, read); p.Percent != nil || p.Metrics == nil || p.Metrics.Supported {
		t.Errorf("unsupported: %+v", p)
	}
}

// Issue #227: the reason of a failed training is the last line of the log that names an error.
func TestLastErrorLine(t *testing.T) {
	log := "Epoch 1\nTraceback (most recent call last):\n  File \"x.py\"\n RuntimeError: CUDA out of memory \nexit 1\n"
	if got := LastErrorLine(log); got != "RuntimeError: CUDA out of memory" {
		t.Errorf("got %q", got)
	}
	if got := LastErrorLine("Epoch 1\nEpoch 2\n"); got != "" {
		t.Errorf("no error line: %q", got)
	}
}

func TestTrainingContainerOf(t *testing.T) {
	for _, c := range []struct{ kind, ref, want string }{
		{kindTrainingRun, "145", "145"},
		{kindPlatformTask, "task:5|train:145", "145"},
		{kindPlatformTask, "task:5|config:12", ""},
		{kindPlatformTask, "task:5", ""},
		{"notebook_run", "r1", ""},
	} {
		if got := TrainingContainerOf(c.kind, c.ref); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.kind, c.ref, got, c.want)
		}
	}
}

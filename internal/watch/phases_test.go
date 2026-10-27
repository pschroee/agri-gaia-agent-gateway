// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"strconv"
	"testing"
)

// Issue #246: a training run names its phase for the history of the job.
func TestTrainingPhases(t *testing.T) {
	metrics := func(epoch, epochs int) string {
		return `{"supported":true,"epochs":[{"epoch":` + itoa(epoch) + `,"epochs":` + itoa(epochs) + `,"val_acc":0.5}]}`
	}
	cases := []struct {
		name, status, score, metrics, phase string
	}{
		{"created", "created", "null", `{}`, "Pulling the image"},
		{"running without figures", "running", "null", `{}`, "Training"},
		{"running epoch 3 of 10", "running", "null", metrics(3, 10), "Training (epoch 3/10)"},
		{"last epoch in, still running", "running", "null", metrics(10, 10), "Saving the model"},
		{"exited with a score", "exited", "0.9", metrics(10, 10), "Finished"},
		{"dead", "dead", "null", `{}`, "Failed"},
	}
	for _, c := range cases {
		read, _ := recorded(t, map[string]string{
			"GET /train/containers/9/status":        `{"status":"` + c.status + `","score":` + c.score + `}`,
			"GET /train/containers/9/logs?tail=100": `{"logs":""}`,
			"GET /train/containers/9/metrics":       c.metrics,
		})
		p, err := pollTrainingRun(context.Background(), Job{ID: "w1", Kind: kindTrainingRun, Ref: "9"}, read)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Phase != c.phase {
			t.Errorf("%s: phase %q, want %q", c.name, p.Phase, c.phase)
		}
	}
}

// Issue #246: the message of the build task that names the container stays as the backend writes it, because
// the plan step and builtContainerRe depend on it; the history takes it as the last phase.
func TestBuiltContainerMessageUnchanged(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/5":                     `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`,
		"GET /train/containers?limit=1000": containersList,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5|config:59"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.Message != "Train Container #63 created." || p.Phase != "" || p.Created == nil || p.Created.ID != "63" {
		t.Fatalf("%+v", p)
	}
	// A phase of the build task passes through as the message, untouched.
	read, _ = recorded(t, map[string]string{
		"GET /tasks/5": `{"status":"inprogress","completion_percentage":0.4,"message":"Building the image · step 3/12 · RUN pip install"}`,
	})
	p, err = pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5|config:59"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateRunning || p.Message != "Building the image · step 3/12 · RUN pip install" {
		t.Fatalf("%+v", p)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

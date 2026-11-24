// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
)

const containersList = `[{"id":62,"dataset_id":7},{"id":63,"dataset_id":59}]`

// #169: the reference of a training build carries the dataset the call named.
func TestTaskRefCarriesDataset(t *testing.T) {
	res := platform.Result{Location: "/tasks/5"}
	for body, want := range map[string]string{
		`{"dataset_id":59,"provider":"x"}`: "task:5|config:59",
		`{"dataset_id":"59"}`:              "task:5|config:59",
		`{"provider":"x"}`:                 "task:5",
		`{"dataset_id":"59; rm -rf /"}`:    "task:5",
	} {
		ref, ok := taskRef(platform.Request{Body: []byte(body)}, res)
		if !ok || ref != want {
			t.Errorf("%s: %q %v, want %q", body, ref, ok, want)
		}
	}
}

func TestPollConfigTaskNamesContainer(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/5":                     `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`,
		"GET /train/containers?limit=1000": containersList,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5|config:59"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Created == nil || p.Created.Resource != delegation.Training || p.Created.ID != "63" {
		t.Fatalf("%+v", p)
	}
	if len(p.Outputs) != 1 || p.Outputs[0].ID != "63" {
		t.Errorf("outputs: %+v", p.Outputs)
	}
}

func TestPollConfigTaskWithoutMessage(t *testing.T) {
	read, calls := recorded(t, map[string]string{
		"GET /tasks/5": `{"status":"completed","completion_percentage":1.0,"message":null}`,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5|config:59"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Created != nil || len(calls()) != 1 {
		t.Fatalf("%+v %v", p, calls())
	}
}

func TestPollConfigTaskContainerNotListed(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/5":                     `{"status":"completed","completion_percentage":1.0,"message":"Train Container #99 created."}`,
		"GET /train/containers?limit=1000": containersList,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5|config:59"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Created != nil || !strings.Contains(p.Message, "does not show it") {
		t.Fatalf("%+v", p)
	}
}

func TestPollConfigTaskWrongDataset(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/5":                     `{"status":"completed","completion_percentage":1.0,"message":"Train Container #62 created."}`,
		"GET /train/containers?limit=1000": containersList,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5|config:59"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.Created != nil {
		t.Fatalf("container of another dataset must not count: %+v", p.Created)
	}
}

func TestPollOldTaskRefUnchanged(t *testing.T) {
	read, calls := recorded(t, map[string]string{
		"GET /tasks/5": `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Created != nil || len(calls()) != 1 {
		t.Fatalf("%+v %v", p, calls())
	}
}

func TestImageBuildCreatedID(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/8":                          `{"status":"completed","completion_percentage":1.0}`,
		"GET /container-images/agent-test/m:v1": `[{"id":14}]`,
	})
	p, err := pollImageBuild(context.Background(), Job{ID: "w1", Kind: kindImageBuild, Ref: "task:8|image:agent-test/m:v1"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Created == nil || p.Created.Resource != delegation.ContainerImage || p.Created.ID != "14" {
		t.Fatalf("%+v", p)
	}
}

func TestImageBuildWaitsForRegistry(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/8": `{"status":"completed","completion_percentage":1.0}`,
	})
	now := time.Unix(1_800_000_000, 0)
	watchClock = func() time.Time { return now }
	t.Cleanup(func() { watchClock = time.Now })
	p, err := pollImageBuild(context.Background(), Job{ID: "w1", Kind: kindImageBuild, Ref: "task:8|image:agent-test/m:v1"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateRunning || p.Next == nil || p.Next.Kind != kindImageBuild ||
		p.Next.Ref != "registry:agent-test/m:v1@1800000000" {
		t.Fatalf("%+v", p)
	}
}

func TestImageRegistryWaitTimesOut(t *testing.T) {
	read, _ := recorded(t, nil)
	watchClock = func() time.Time { return time.Unix(1_800_000_000, 0).Add(registryWait + time.Second) }
	t.Cleanup(func() { watchClock = time.Now })
	p, err := pollImageBuild(context.Background(), Job{ID: "w1", Kind: kindImageBuild, Ref: "registry:agent-test/m:v1@1800000000"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateFailed || !strings.Contains(p.Message, "lists no image agent-test/m:v1") {
		t.Fatalf("%+v", p)
	}
}

func TestImageSeveralIDs(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /container-images/agent-test/m:v1": `[{"id":14},{"id":15}]`,
	})
	p, err := pollImageBuild(context.Background(), Job{ID: "w1", Kind: kindImageBuild, Ref: "registry:agent-test/m:v1@1800000000"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateDone || p.Created != nil || !strings.Contains(p.Message, "several images") {
		t.Fatalf("%+v", p)
	}
}

// Answers of pilot-bm17-c6 (23.11.2026, run s3-outdated): the task of the build already named container 188
// while it was still "inprogress" (tool call 14:24:22 UTC), and the list showed 188 on dataset 473. The
// agent's start of 188 at 14:25:18 was refused as a foreign object (platform_calls 28716), because the
// container entered the register only once the task counted as completed (between 14:26:45 and 14:29:19).
const (
	c6TaskInProgress = `{"completion_percentage":0.95,"creation_date":"2026-11-23T14:09:43.960751","id":342,"initiator":"agent-exp","message":"Train Container #188 created.","status":"inprogress","title":"Train Container Buildjob: YOLOv11 (Ultralytics)"}`
	c6Containers     = `[{"id":63,"dataset_id":60},{"id":97,"dataset_id":193},{"id":98,"dataset_id":195},{"id":188,"dataset_id":473}]`
)

func TestPollConfigTaskNamesContainerWhileRunning(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/342":                   c6TaskInProgress,
		"GET /train/containers?limit=1000": c6Containers,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:342|config:473"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateRunning {
		t.Fatalf("the job must keep running until the task completes: %+v", p)
	}
	if p.Created == nil || p.Created.Resource != delegation.Training || p.Created.ID != "188" {
		t.Fatalf("the container the task names must count as created: %+v", p.Created)
	}
	if len(p.Outputs) != 0 {
		t.Errorf("outputs belong to the finished job: %+v", p.Outputs)
	}
}

func TestPollConfigTaskRunningChecksDataset(t *testing.T) {
	read, _ := recorded(t, map[string]string{
		"GET /tasks/342":                   c6TaskInProgress,
		"GET /train/containers?limit=1000": c6Containers,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:342|config:60"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateRunning || p.Created != nil {
		t.Fatalf("a container on another dataset must not count: %+v", p)
	}
}

func TestPollConfigTaskRunningWithoutMessageReadsNoList(t *testing.T) {
	read, calls := recorded(t, map[string]string{
		"GET /tasks/342": `{"completion_percentage":0.9,"creation_date":"2026-11-23T14:09:43.960751","id":342,"initiator":"agent-exp","message":"Pushing the image to the registry","status":"inprogress","title":"Train Container Buildjob: YOLOv11 (Ultralytics)"}`,
	})
	p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:342|config:473"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateRunning || p.Created != nil || len(calls()) != 1 {
		t.Fatalf("%+v %v", p, calls())
	}
}

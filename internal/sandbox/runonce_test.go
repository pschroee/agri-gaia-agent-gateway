// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The tmpfs and the label of a check container (R0.11); they are repeated here instead of imported,
// because internal/assess uses this package and no test may close that circle.
const (
	checkTmpfs = "rw,noexec,nosuid,size=1g"
	checkLabel = "agw.check"
)

// RunOnce runs one command in a container of its own and removes it again (R0.10). Integration test
// against the local Docker daemon and the image of the execution sandbox: runs only with
// AGW_DOCKER_TESTS=1, and needs an image with python3 (AGW_IMAGE, default agwpoc/agw-basis:dev).
func TestRunOnceRemovesContainer(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	rt, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := "agw-check-test-" + time.Now().Format("150405")
	// The entrypoint of the execution image is `agw-exec idle`, which ignores arguments and never ends:
	// the check runs its script directly (Spec.Entrypoint). The command reads one line and not to EOF:
	// the write side of the connection stays open while the answer is read (see RunOnce).
	spec := Spec{
		Name: name, Image: image, Entrypoint: []string{"python3", "-c", "import sys; print(sys.stdin.readline().strip().upper())"},
		InternalNet: "none", MemoryMB: 256, CPUs: 1, Pids: 32,
		Tmpfs:  map[string]string{"/tmp": checkTmpfs},
		Labels: map[string]string{checkLabel: "1"},
	}
	out, exit, stderr, err := rt.RunOnce(ctx, spec, strings.NewReader("hello check\n"), 1<<20)
	if err != nil || exit != 0 {
		t.Fatalf("RunOnce: %v (exit %d, stderr %q)", err, exit, stderr)
	}
	if string(out) != "HELLO CHECK\n" {
		t.Fatalf("answer: %q", out)
	}
	// The container is gone, whatever happened.
	if ids, err := rt.FindByLabels(ctx, map[string]string{checkLabel: "1"}); err != nil {
		t.Fatal(err)
	} else if len(ids) != 0 {
		t.Errorf("the check container is still there: %v", ids)
	}
	// An exit code comes back without an error, so that the caller can show what the command said.
	spec.Name = name + "-fail"
	spec.Entrypoint = []string{"python3", "-c", "import sys; sys.stderr.write('Traceback: boom\\n'); sys.exit(3)"}
	if _, exit, stderr, err := rt.RunOnce(ctx, spec, strings.NewReader(""), 1<<20); err != nil || exit != 3 || !strings.Contains(stderr, "Traceback") {
		t.Errorf("failing command: %v (exit %d, stderr %q)", err, exit, stderr)
	}
	// A time limit ends the run and removes the container.
	spec.Name = name + "-slow"
	spec.Entrypoint = []string{"python3", "-c", "import time; time.sleep(60)"}
	slow, cancelSlow := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSlow()
	if _, _, _, err := rt.RunOnce(slow, spec, strings.NewReader(""), 1<<20); err == nil {
		t.Error("a run past its time limit returns no error")
	}
	if ids, err := rt.FindByLabels(ctx, map[string]string{checkLabel: "1"}); err != nil {
		t.Fatal(err)
	} else if len(ids) != 0 {
		t.Errorf("a container of a run past its limit stays: %v", ids)
	}
	// More output than allowed is an error, not a longer answer.
	spec.Name = name + "-loud"
	spec.Entrypoint = []string{"python3", "-c", "print('x' * 10000)"}
	if _, _, _, err := rt.RunOnce(ctx, spec, strings.NewReader(""), 100); err == nil ||
		!strings.Contains(err.Error(), "check output larger than 100 bytes") {
		t.Errorf("too much output: %v", err)
	}
}

// The whole answer arrives, also when the command writes it after a pause. Closing the write side of the
// hijacked connection early ends the whole stream: the live test of R0 got an empty answer with an exit
// code of 0 and a container that was still running (see RunOnce).
func TestRunOnceKeepsLateOutput(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	rt, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, exit, stderr, err := rt.RunOnce(ctx, Spec{
		Name: "agw-check-late-" + time.Now().Format("150405"), Image: image,
		Entrypoint:  []string{"/bin/sh", "-c", "echo EARLY; sleep 2; echo LATE"},
		InternalNet: "none", MemoryMB: 256, CPUs: 1, Pids: 32,
		Tmpfs:  map[string]string{"/tmp": checkTmpfs},
		Labels: map[string]string{checkLabel: "1"},
	}, strings.NewReader(""), 1<<20)
	if err != nil || exit != 0 {
		t.Fatalf("RunOnce: %v (exit %d, stderr %q)", err, exit, stderr)
	}
	if string(out) != "EARLY\nLATE\n" {
		t.Fatalf("the answer lost what was written later: %q", out)
	}
}

// The command reads its input to its end, not to EOF: the write side stays open while the answer is read,
// and /opt/agw/check/predict.py stops at the end-of-archive marker of its tar exactly like this.
func TestRunOnceTarInputWithoutEOF(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	rt, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	blob := tarWith(t, map[string]string{"job.json": "{}\n", "model.onnx": "onnx", "images/abc.png": "PNG"})
	out, exit, stderr, err := rt.RunOnce(ctx, Spec{
		Name: "agw-check-tar-" + time.Now().Format("150405"), Image: image,
		Entrypoint: []string{"python3", "-c",
			"import sys, tarfile; tf = tarfile.open(fileobj=sys.stdin.buffer, mode='r|'); print(' '.join(m.name for m in tf))"},
		InternalNet: "none", MemoryMB: 256, CPUs: 1, Pids: 32,
		Tmpfs:  map[string]string{"/tmp": checkTmpfs},
		Labels: map[string]string{checkLabel: "1"},
	}, bytes.NewReader(blob), 1<<20)
	if err != nil || exit != 0 {
		t.Fatalf("RunOnce: %v (exit %d, stderr %q)", err, exit, stderr)
	}
	if got := strings.TrimSpace(string(out)); got != "job.json model.onnx images/abc.png" {
		t.Fatalf("the command read %q", got)
	}
}

// tarWith builds a small tar for a test (the same shape assess.BuildTar writes; this package cannot
// import internal/assess, which uses it).
func tarWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"job.json", "model.onnx", "images/abc.png"} {
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	// Padded to a whole record like tar(1) and assess.BuildTar: a record-oriented reader (tarfile) would
	// otherwise wait for the rest of its last record, and the write side of the connection stays open.
	if rest := buf.Len() % tarRecord; rest != 0 {
		buf.Write(make([]byte, tarRecord-rest))
	}
	return buf.Bytes()
}

// tarRecord is the record size of a tar archive (tar(1)'s blocking factor 20 × 512, tarfile.RECORDSIZE).
const tarRecord = 20 * 512

// The image of the execution sandbox holds the check script and onnxruntime (R0.15); the agent can read
// the script but not change it.
func TestCheckScriptInImage(t *testing.T) {
	if os.Getenv("AGW_DOCKER_TESTS") != "1" {
		t.Skip("set AGW_DOCKER_TESTS=1 to test against Docker")
	}
	image := os.Getenv("AGW_IMAGE")
	if image == "" {
		image = "agwpoc/agw-basis:dev"
	}
	rt, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ins, err := rt.Start(ctx, Spec{Name: "agw-check-script-" + time.Now().Format("150405"), Image: image, NoAttach: true,
		InternalNet: "none", MemoryMB: 512, CPUs: 1, Pids: 32, Tmpfs: ExecTmpfs})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Remove(context.Background(), ins.ID) }()
	out, _, err := rt.Exec(ctx, ins.ID, []string{"python3", "-c",
		"import onnxruntime, PIL, os; print(os.stat('/opt/agw/check/predict.py').st_size > 0, onnxruntime.__version__)"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "True") {
		t.Errorf("the check script or onnxruntime is missing: %s", out)
	}
	// The script belongs to the image, not to the agent: it is not writable for the agent (uid 10001).
	if _, _, err := rt.Exec(ctx, ins.ID, []string{"python3", "-c",
		"open('/opt/agw/check/predict.py','w').write('x')"}, nil); err == nil {
		t.Error("the agent can write the check script")
	}
}

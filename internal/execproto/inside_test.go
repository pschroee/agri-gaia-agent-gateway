// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package execproto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInside(t *testing.T) {
	for _, c := range []struct {
		p    string
		want bool
	}{
		{"/workspace", true},
		{"/workspace/a.txt", true},
		{"/workspace/sub/../a.txt", true},
		{"/workspace/../etc/passwd", false},
		{"/workspacex/a.txt", false},
		{"/etc/passwd", false},
	} {
		if got := Inside("/workspace", c.p); got != c.want {
			t.Errorf("Inside(%q) = %v", c.p, got)
		}
	}
}

// Issue #62: files sent to the user come only from inside the workspace, as regular files reached without symbolic
// links; a FIFO does not hold the call.
func TestOpenInside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "data.csv"), []byte("a,b\n"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "out"), 0o755))
	must(os.WriteFile(filepath.Join(root, "out", "chart.png"), []byte("png"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644))
	must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")))
	must(os.Symlink(filepath.Join(root, "data.csv"), filepath.Join(root, "inner-link.csv")))
	must(os.Symlink(outside, filepath.Join(root, "dirlink")))
	must(syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644))

	for _, ok := range []string{filepath.Join(root, "data.csv"), filepath.Join(root, "out", "chart.png")} {
		f, err := OpenInside(root, ok)
		if err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
		f.Close()
	}
	// relative to the current directory
	wd, _ := os.Getwd()
	must(os.Chdir(root))
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if f, err := OpenInside(root, "out/chart.png"); err != nil {
		t.Fatalf("relative: %v", err)
	} else {
		f.Close()
	}

	for _, c := range []struct{ path, want string }{
		{filepath.Join(outside, "secret.txt"), "only files inside"},
		{filepath.Join(root, "..", filepath.Base(outside), "secret.txt"), "only files inside"},
		{filepath.Join(root, "dirlink", "secret.txt"), "only files inside"},
		{filepath.Join(root, "link.txt"), "symbolic link"},
		{filepath.Join(root, "inner-link.csv"), "symbolic link"},
		{filepath.Join(root, "pipe"), "not a regular file"},
		{filepath.Join(root, "out"), "not a regular file"},
		{filepath.Join(root, "missing.txt"), "no such file"},
		{"", "no file given"},
	} {
		f, err := OpenInside(root, c.path)
		if err == nil {
			f.Close()
			t.Fatalf("%s: opened", c.path)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v (want %q)", c.path, err, c.want)
		}
		if c.want == "only files inside" && !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("%s: not ErrOutsideRoot: %v", c.path, err)
		}
	}
}

func TestValidateRoot(t *testing.T) {
	r := Request{Op: OpRead, Path: "/workspace/a/../b.txt", Root: "/workspace/"}
	if err := r.Validate(); err != nil || r.Root != "/workspace" || r.Path != "/workspace/b.txt" {
		t.Fatalf("valid: %v %+v", err, r)
	}
	r = Request{Op: OpRead, Path: "/tmp/b.txt", Root: "/workspace"}
	if err := r.Validate(); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("outside: %v", err)
	}
	r = Request{Op: OpWrite, Path: "/workspace/b.txt", Root: "/workspace"}
	if err := r.Validate(); err == nil {
		t.Fatal("root accepted for write")
	}
	r = Request{Op: OpRead, Path: "/workspace/b.txt", Root: "workspace"}
	if err := r.Validate(); err == nil {
		t.Fatal("relative root accepted")
	}
}

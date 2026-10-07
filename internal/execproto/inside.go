// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package execproto

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Workspace is the working directory of the execution sandbox. Files the agent sends to the user (issue #62) must
// lie inside it.
const Workspace = "/workspace"

// ErrOutsideRoot: the path leaves the permitted directory (also through a symbolic link in between).
var ErrOutsideRoot = errors.New("outside the permitted directory")

// Inside reports whether the cleaned absolute path p is root itself or lies below it (no I/O).
func Inside(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// OpenInside opens the regular file p for reading only if it lies inside root (issue #62, the checks of the other
// helpers): the directories on the way are resolved and must stay inside root, the file itself must not be a
// symbolic link (O_NOFOLLOW) and must be a regular file; O_NONBLOCK keeps a FIFO from holding the call (Review 3, N1).
// A relative p counts from the current directory.
func OpenInside(root, p string) (*os.File, error) {
	if p == "" {
		return nil, errors.New("no file given")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", root, err)
	}
	outside := fmt.Errorf("only files inside %s can be sent, not %s: %w", root, p, ErrOutsideRoot)
	if !Inside(root, abs) && !Inside(realRoot, abs) {
		return nil, outside
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	if !Inside(realRoot, dir) {
		return nil, outside
	}
	full := filepath.Join(dir, filepath.Base(abs))
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("%s is a symbolic link; send the file itself", p)
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return f, nil
}

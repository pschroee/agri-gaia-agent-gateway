// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

//go:build unix

package main

import (
	"os/signal"
	"syscall"
)

// hookSysProcAttr puts the hook program into a process group of its own, so that the time limit can kill it and
// everything it started.
func hookSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// killGroup kills the process group of the hook program.
func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }

// ignoreSIGPIPE keeps the runner alive when the reader of its stdout or stderr goes away (Go ends a process that
// writes to a closed pipe on fd 1 or 2).
func ignoreSIGPIPE() { signal.Ignore(syscall.SIGPIPE) }

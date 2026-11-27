// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

//go:build !unix

package main

import (
	"os"
	"syscall"
)

func hookSysProcAttr() *syscall.SysProcAttr { return nil }

// killGroup ends the hook program itself; there are no process groups here.
func killGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func ignoreSIGPIPE() {}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// dropTraced switches a traced operation to the agent user. The supervisor sets
// AGW_EXEC_SETUID/AGW_EXEC_SETGID only when it starts the operation under strace: the tracer
// must have the same uid as its tracee when it attaches, so the switch happens in the helper
// rather than in strace (X6.1; a switch by strace before attaching needs CAP_SYS_PTRACE).
func dropTraced() error {
	u := os.Getenv("AGW_EXEC_SETUID")
	if u == "" {
		return nil
	}
	uid, err := strconv.Atoi(u)
	if err != nil || uid < 0 {
		return fmt.Errorf("invalid AGW_EXEC_SETUID %q", u)
	}
	gid := uid
	if g := os.Getenv("AGW_EXEC_SETGID"); g != "" {
		if gid, err = strconv.Atoi(g); err != nil || gid < 0 {
			return fmt.Errorf("invalid AGW_EXEC_SETGID %q", g)
		}
	}
	// The agent's processes must not inherit the values; they are meaningless outside.
	_ = os.Unsetenv("AGW_EXEC_SETUID")
	_ = os.Unsetenv("AGW_EXEC_SETGID")
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	if os.Getuid() != uid || os.Getgid() != gid {
		return fmt.Errorf("still uid %d gid %d after the switch", os.Getuid(), os.Getgid())
	}
	return nil
}

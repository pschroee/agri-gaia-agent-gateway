package main

import "syscall"

// notDumpable sets PR_SET_DUMPABLE to 0: /proc/<pid>/fd then belongs to root, and another
// process of the same user (the agent's bash) cannot write to this operation's output via
// /proc/<pid>/fd/1 (code review L7, security review N1). An execve (e.g. bash)
// resets the value to 1 for the new program.
func notDumpable() error {
	const prSetDumpable = 4
	_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

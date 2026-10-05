package main

import "syscall"

// notDumpable setzt PR_SET_DUMPABLE auf 0: /proc/<pid>/fd gehört dann root, und ein anderer
// Prozess desselben Nutzers (bash des Agenten) kann die Ausgabe dieser Operation nicht über
// /proc/<pid>/fd/1 beschreiben (Code-Review L7, Security-Review N1). Ein execve (etwa bash)
// setzt den Wert für das neue Programm wieder auf 1.
func notDumpable() error {
	const prSetDumpable = 4
	_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

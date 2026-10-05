// agw-exec: static helper for both containers of a slot (E9).
//
// In the execution sandbox:
//
//	agw-exec idle            PID 1: waits and reaps orphaned processes
//	agw-exec serve           supervisor (as root via docker exec), protocol execproto over stdin/stdout
//	agw-exec op              child process of the supervisor, one operation as the agent user
//
// In pi's container (without a shell):
//
//	agw-exec pi-entry ARGS   entrypoint: write the configuration, start pi in RPC mode (execve)
//	agw-exec put PATH        write stdin into a file (session, configuration)
//	agw-exec poll-subagents  new lines from the subagents' session files (JSON on stdin/stdout)
//	agw-exec kill-node       end all node processes except pi (PID 1)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"agw/internal/execproto"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agw-exec idle|serve|op|pi-entry|put|poll-subagents|kill-node")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "idle":
		idle()
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		uid := fs.Int("uid", 10001, "agent user (-1: do not switch)")
		gid := fs.Int("gid", 10001, "group of the agent")
		bgMax := fs.Int("bg-max", execproto.DefaultBgMax, "at most this many concurrent background tasks")
		_ = fs.Parse(os.Args[2:])
		self, err := os.Executable()
		if err != nil {
			self = "/usr/local/bin/agw-exec"
		}
		if os.Getuid() != 0 {
			*uid = -1
		}
		srv := newServer(os.Stdout, self, *uid, *gid)
		srv.bgMax = max(1, *bgMax)
		if *uid >= 0 {
			// Create it before the agent's first command, so that it cannot plant anything there beforehand.
			if err := ensureBgDir(filepath.Dir(bgLogFile(execproto.BgLogPath(1)))); err != nil {
				fmt.Fprintln(os.Stderr, "agw-exec serve: directory of the background tasks:", err)
			}
		}
		srv.run(os.Stdin)
	case "op":
		os.Exit(opMain())
	case "pi-entry":
		if err := piEntry(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "agw-exec pi-entry:", err)
			os.Exit(1)
		}
	case "put":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		if err := put(os.Args[2], os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "agw-exec put:", err)
			os.Exit(1)
		}
	case "poll-subagents":
		if err := pollSubagents(os.Stdin, os.Stdout, "/agent/sessions", "/tmp/pi-subagents-uid-*/async-subagent-runs/*/status.json"); err != nil {
			fmt.Fprintln(os.Stderr, "agw-exec poll-subagents:", err)
			os.Exit(1)
		}
	case "kill-node":
		fmt.Println(killNode("/proc"))
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

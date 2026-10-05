// agw-exec: statischer Helfer für beide Container eines Platzes (E9).
//
// In der Ausführungs-Sandbox:
//
//	agw-exec idle            PID 1: wartet und räumt verwaiste Prozesse ab
//	agw-exec serve           Überwacher (als root per docker exec), Protokoll execproto über stdin/stdout
//	agw-exec op              Kindprozess des Überwachers, eine Operation als Agent-Nutzer
//
// Im Container von pi (ohne Shell):
//
//	agw-exec pi-entry ARGS   Entrypoint: Konfiguration schreiben, pi im RPC-Modus starten (execve)
//	agw-exec put PFAD        stdin in eine Datei schreiben (Sitzung, Konfiguration)
//	agw-exec poll-subagents  neue Zeilen aus den Sitzungsdateien der Subagenten (JSON auf stdin/stdout)
//	agw-exec kill-node       alle node-Prozesse außer pi (PID 1) beenden
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
		fmt.Fprintln(os.Stderr, "Aufruf: agw-exec idle|serve|op|pi-entry|put|poll-subagents|kill-node")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "idle":
		idle()
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		uid := fs.Int("uid", 10001, "Agent-Nutzer (-1: nicht wechseln)")
		gid := fs.Int("gid", 10001, "Gruppe des Agenten")
		bgMax := fs.Int("bg-max", execproto.DefaultBgMax, "höchstens so viele Hintergrundaufgaben gleichzeitig")
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
			// Vor dem ersten Befehl des Agenten anlegen, damit er dort nichts vorab ablegen kann.
			if err := ensureBgDir(filepath.Dir(bgLogFile(execproto.BgLogPath(1)))); err != nil {
				fmt.Fprintln(os.Stderr, "agw-exec serve: Verzeichnis der Hintergrundaufgaben:", err)
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
		fmt.Fprintln(os.Stderr, "unbekannter Befehl", os.Args[1])
		os.Exit(2)
	}
}

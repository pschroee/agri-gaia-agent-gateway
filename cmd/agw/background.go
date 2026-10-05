package main

import (
	"fmt"
	"strings"
	"time"

	"agw/internal/agwclient"
)

// cmdChatBg shows a chat's background tasks with the last lines of their output.
func (a *app) cmdChatBg(args []string) error {
	fs := a.flags("chat bg")
	tail := fs.Int("tail", 3, "this many last lines per task (0: none)")
	pos, err := a.parse(fs, args, 1, 1, "agw chat bg <id> [--tail N] [--json]")
	if err != nil {
		return err
	}
	list, err := a.c.Background(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(list)
	}
	if len(list) == 0 {
		fmt.Fprintln(a.stdout, "No background tasks.")
		return nil
	}
	for _, t := range list {
		fmt.Fprintf(a.stdout, "%s  %s  %s  %s\n", t.ID, bgStateLabel(t), bgRuntime(t, time.Now()), truncateLine(t.Command, 70))
		if t.Session != "" && t.Session != "main" {
			fmt.Fprintf(a.stdout, "    Subagent %s\n", t.Session)
		}
		if *tail > 0 {
			for _, l := range lastLines(t.Tail, *tail) {
				fmt.Fprintf(a.stdout, "    │ %s\n", truncateLine(l, 110))
			}
		}
	}
	return nil
}

// cmdChatBgStop stops a running background task.
func (a *app) cmdChatBgStop(args []string) error {
	fs := a.flags("chat bg-stop")
	pos, err := a.parse(fs, args, 2, 2, "agw chat bg-stop <id> <bg-id> [--json]")
	if err != nil {
		return err
	}
	t, err := a.c.StopBackground(a.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(t)
	}
	fmt.Fprintf(a.stdout, "%s: %s\n", t.ID, bgStateLabel(t))
	return nil
}

func bgStateLabel(t agwclient.BackgroundTask) string {
	switch t.State {
	case "running":
		return "running"
	case "exited":
		if t.ExitCode != nil {
			return fmt.Sprintf("exited (exit %d)", *t.ExitCode)
		}
		return "exited"
	case "timeout":
		return "timeout"
	case "stopped":
		if t.StoppedBy == "user" {
			return "stopped by the user"
		}
		return "stopped by the agent"
	case "suspended":
		return "ended when going idle"
	case "closed":
		return "ended with the chat"
	case "lost":
		return "lost with the sandbox"
	}
	if t.Error != "" {
		return "failed: " + truncateLine(t.Error, 60)
	}
	return "failed"
}

// bgRuntime: runtime as m:ss (until now while the task is running).
func bgRuntime(t agwclient.BackgroundTask, now time.Time) string {
	start, err := time.Parse(time.RFC3339Nano, t.StartedAt)
	if err != nil {
		return "?"
	}
	end := now
	if e, err := time.Parse(time.RFC3339Nano, t.EndedAt); err == nil {
		end = e
	}
	s := int(end.Sub(start).Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func lastLines(s string, n int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return l
}

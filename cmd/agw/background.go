package main

import (
	"fmt"
	"strings"
	"time"

	"agw/internal/agwclient"
)

// cmdChatBg zeigt die Hintergrundaufgaben eines Chats mit den letzten Zeilen ihrer Ausgabe.
func (a *app) cmdChatBg(args []string) error {
	fs := a.flags("chat bg")
	tail := fs.Int("tail", 3, "so viele letzte Zeilen je Aufgabe (0: keine)")
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
		fmt.Fprintln(a.stdout, "Keine Hintergrundaufgaben.")
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

// cmdChatBgStop beendet eine laufende Hintergrundaufgabe.
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
		return "läuft"
	case "exited":
		if t.ExitCode != nil {
			return fmt.Sprintf("beendet (Exit %d)", *t.ExitCode)
		}
		return "beendet"
	case "timeout":
		return "Zeitgrenze"
	case "stopped":
		if t.StoppedBy == "user" {
			return "vom Nutzer gestoppt"
		}
		return "vom Agenten gestoppt"
	case "suspended":
		return "beim Ruhen beendet"
	case "closed":
		return "mit dem Chat beendet"
	case "lost":
		return "mit der Sandbox verloren"
	}
	if t.Error != "" {
		return "fehlgeschlagen: " + truncateLine(t.Error, 60)
	}
	return "fehlgeschlagen"
}

// bgRuntime: Laufzeit als m:ss (bis jetzt, solange die Aufgabe läuft).
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

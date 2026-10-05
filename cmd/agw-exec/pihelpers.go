package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// --- Container von pi ---

const (
	nodeBin = "/usr/local/bin/node"
	piCLI   = "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
)

// piEntry ersetzt das frühere entrypoint.sh (P3: pi braucht keine Shell).
// Die Konfiguration kommt base64-kodiert aus der Umgebung und landet im
// tmpfs unter /agent; danach ersetzt sich der Prozess durch pi, das damit
// PID 1 wird.
func piEntry(args []string) error {
	for _, d := range []string{"/agent/config", "/agent/sessions"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for env, file := range map[string]string{"AGW_PI_MODELS_JSON": "models.json", "AGW_PI_SETTINGS_JSON": "settings.json"} {
		if v := os.Getenv(env); v != "" {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return errors.New(env + ": " + err.Error())
			}
			if err := os.WriteFile(filepath.Join("/agent/config", file), b, 0o644); err != nil {
				return err
			}
		}
		_ = os.Unsetenv(env)
	}
	// Beim Bau installierte pi-Pakete (pi-subagents, rpiv-todo) sichtbar machen.
	_ = os.Remove("/agent/config/npm")
	if err := os.Symlink("/opt/agw/pihome/npm", "/agent/config/npm"); err != nil {
		return err
	}
	_ = os.Setenv("PI_CODING_AGENT_DIR", "/agent/config")
	_ = os.Setenv("PI_OFFLINE", "1")
	if err := os.Chdir("/workspace"); err != nil {
		return err
	}
	argv := append([]string{"node", piCLI, "--mode", "rpc", "--offline", "--no-context-files", "--no-skills",
		"--no-extensions", "--session-dir", "/agent/sessions"}, args...)
	return syscall.Exec(nodeBin, argv, os.Environ())
}

// put schreibt stdin nach p (Eltern werden angelegt), höchstens 256 MiB.
func put(p string, r io.Reader) error {
	if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return errors.New("Pfad muss absolut sein")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(r, 256<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 256<<20 {
		return errors.New("zu groß")
	}
	tmp := p + ".agw-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

var inputRe = regexp.MustCompile(`^([0-9a-f-]{36})_(.+?)(?:_\d+)?_input\.md$`)

// pollSubagents liest neue, vollständige Zeilen aus den Sitzungsdateien der
// Subagenten ab den übergebenen Positionen (früher ein Python-Skript; im
// Container von pi gibt es kein Python mehr).
func pollSubagents(in io.Reader, out io.Writer, root, statusGlob string) error {
	var req struct {
		Offsets map[string]int64 `json:"offsets"`
	}
	if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&req); err != nil && err != io.EOF {
		return err
	}
	type file struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Data   string `json:"data"`
	}
	res := struct {
		Files  []file             `json:"files"`
		Agents map[string]string  `json:"agents"`
		Runs   map[string]runInfo `json:"runs"`
	}{Files: []file{}, Agents: map[string]string{}, Runs: map[string]runInfo{}}
	paths, _ := filepath.Glob(filepath.Join(root, "*", "*", "run-*", "session.jsonl"))
	sort.Strings(paths)
	if len(paths) > 64 {
		paths = paths[:64]
	}
	for _, p := range paths {
		o := req.Offsets[p]
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil || st.Size() <= o {
			f.Close()
			continue
		}
		buf := make([]byte, min(st.Size()-o, 262144))
		n, _ := f.ReadAt(buf, o)
		f.Close()
		buf = buf[:n]
		cut := strings.LastIndexByte(string(buf), '\n')
		if cut < 0 {
			continue
		}
		data := buf[:cut+1]
		s := string(data)
		if !utf8.ValidString(s) {
			s = strings.ToValidUTF8(s, "�")
		}
		res.Files = append(res.Files, file{Path: p, Offset: o + int64(len(data)), Data: s})
	}
	inputs, _ := filepath.Glob(filepath.Join(root, "subagent-artifacts", "*_input.md"))
	if len(inputs) > 256 {
		inputs = inputs[:256]
	}
	for _, p := range inputs {
		if m := inputRe.FindStringSubmatch(filepath.Base(p)); m != nil {
			res.Agents[m[1]] = m[2]
		}
	}
	if statusGlob != "" {
		res.Runs = subagentRuns(statusGlob)
	}
	return json.NewEncoder(out).Encode(res)
}

// runInfo: Name und Zustand eines Subagenten-Laufs aus den Statusdateien von pi-subagents
// (async-subagent-runs/<Lauf>/status.json), geordnet nach der Sitzungsdatei des Kindes. Nur so
// lassen sich die Sitzungsordner (eigene Kennung) den Läufen von pi-subagents zuordnen.
type runInfo struct {
	Agent     string `json:"agent,omitempty"`
	Label     string `json:"label,omitempty"`
	State     string `json:"state,omitempty"`
	PiRun     string `json:"pi_run,omitempty"`
	Parent    string `json:"parent,omitempty"`
	StartedAt int64  `json:"started_at,omitempty"`
	EndedAt   int64  `json:"ended_at,omitempty"`
}

var childSessionRe = regexp.MustCompile(`^/agent/sessions/[^/]+/([0-9a-fA-F-]{8,64})/run-(\d+)/session\.jsonl$`)

// childKey: Kennung des Laufs wie sock.SessionKey (bei parallelen Kindern mit #n).
func childKey(file string) string {
	m := childSessionRe.FindStringSubmatch(file)
	if m == nil {
		return ""
	}
	if m[2] == "0" {
		return m[1]
	}
	return m[1] + "#" + m[2]
}

func subagentRuns(glob string) map[string]runInfo {
	type step struct {
		Agent       string `json:"agent"`
		Label       string `json:"label"`
		Status      string `json:"status"`
		SessionFile string `json:"sessionFile"`
		StartedAt   int64  `json:"startedAt"`
		EndedAt     int64  `json:"endedAt"`
	}
	type status struct {
		RunID       string `json:"runId"`
		Mode        string `json:"mode"`
		State       string `json:"state"`
		WorkflowKey string `json:"workflowKey"`
		Parent      string `json:"parentWorkflowRunId"`
		SessionFile string `json:"sessionFile"`
		StartedAt   int64  `json:"startedAt"`
		EndedAt     int64  `json:"endedAt"`
		Steps       []step `json:"steps"`
	}
	out := map[string]runInfo{}
	paths, _ := filepath.Glob(glob)
	sort.Strings(paths)
	if len(paths) > 256 {
		paths = paths[len(paths)-256:]
	}
	var own []status // Statusdatei des Kindes selbst: gewinnt gegen die Zeile im Workflow
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var st status
		err = json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&st)
		f.Close()
		if err != nil {
			continue
		}
		if st.Mode == "workflow" {
			for _, s := range st.Steps {
				if k := childKey(s.SessionFile); k != "" {
					out[k] = runInfo{Agent: s.Agent, Label: s.Label, State: s.Status, Parent: st.RunID, StartedAt: s.StartedAt, EndedAt: s.EndedAt}
				}
			}
			continue
		}
		own = append(own, st)
	}
	for _, st := range own {
		for i, s := range st.Steps {
			k := childKey(s.SessionFile)
			if k == "" && i == 0 && len(st.Steps) == 1 {
				k = childKey(st.SessionFile)
			}
			if k == "" {
				continue
			}
			label := s.Label
			if label == "" {
				label = st.WorkflowKey
			}
			state := s.Status
			if len(st.Steps) == 1 || state == "" {
				state = st.State
			}
			started, ended := s.StartedAt, s.EndedAt
			if started == 0 {
				started = st.StartedAt
			}
			if ended == 0 && len(st.Steps) == 1 {
				ended = st.EndedAt
			}
			out[k] = runInfo{Agent: s.Agent, Label: label, State: state, PiRun: st.RunID, Parent: st.Parent, StartedAt: started, EndedAt: ended}
		}
	}
	return out
}

// killNode beendet alle node-Prozesse außer PID 1 (pi) und sich selbst.
func killNode(proc string) int {
	ents, _ := os.ReadDir(proc)
	self := os.Getpid()
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 || pid == self {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join(proc, e.Name(), "cmdline"))
		if err != nil || !strings.Contains(strings.ReplaceAll(string(cmd), "\x00", " "), "node") {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			n++
		}
	}
	return n
}

// --- Ausführungs-Sandbox ---

// idle ist PID 1 der Ausführungs-Sandbox: Es tut nichts, räumt aber
// verwaiste Prozesse ab (Hintergrundprozesse des Agenten landen hier).
// PID 1 läuft als Agent-Nutzer; der Agent kann ihm also jedes Signal
// schicken. Deshalb nimmt idle alle Signale an (signal.Notify ohne Liste) und
// verwirft alles außer SIGCHLD: Ohne das beendeten Signale mit Standardaktion
// „Kern“ (QUIT, ABRT, TRAP, SYS, ILL, SEGV, BUS, FPE) oder „Ende“ (STKFLT,
// USR1, …) die Sandbox (Code-Review H2). SIGKILL und SIGSTOP stellt der
// Kernel PID 1 innerhalb des Namensraums nicht zu. Der Orchestrator entfernt
// Container mit SIGKILL von außen.
func idle() {
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs)
	for s := range sigs {
		if s != syscall.SIGCHLD {
			continue
		}
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}

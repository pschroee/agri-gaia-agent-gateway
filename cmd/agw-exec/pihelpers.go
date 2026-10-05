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

// --- pi's container ---

const (
	nodeBin = "/usr/local/bin/node"
	piCLI   = "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
)

// piEntry replaces the former entrypoint.sh (P3: pi needs no shell).
// The configuration comes base64-encoded from the environment and ends up in
// the tmpfs under /agent; afterwards the process replaces itself with pi, which
// thus becomes PID 1.
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
	// Make the pi packages installed at build time (pi-subagents, rpiv-todo) visible.
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

// put writes stdin to p (parents are created), at most 256 MiB.
func put(p string, r io.Reader) error {
	if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return errors.New("path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(r, 256<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 256<<20 {
		return errors.New("too large")
	}
	tmp := p + ".agw-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

var inputRe = regexp.MustCompile(`^([0-9a-f-]{36})_(.+?)(?:_\d+)?_input\.md$`)

// pollSubagents reads new, complete lines from the subagents' session files
// from the given offsets onwards (formerly a Python script; pi's container
// no longer has Python).
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

// runInfo: name and state of a subagent run from pi-subagents' status files
// (async-subagent-runs/<run>/status.json), keyed by the child's session file. Only this way
// can the session folders (own ID) be matched to the runs of pi-subagents.
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

// childKey: ID of the run like sock.SessionKey (with #n for parallel children).
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
	var own []status // the child's own status file: wins over the line in the workflow
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

// killNode kills all node processes except PID 1 (pi) and itself.
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

// --- execution sandbox ---

// idle is PID 1 of the execution sandbox: it does nothing but reap
// orphaned processes (the agent's background processes end up here).
// PID 1 runs as the agent user; the agent can therefore send it any
// signal. That is why idle accepts all signals (signal.Notify without a list) and
// discards everything except SIGCHLD: otherwise signals with the default action
// "core" (QUIT, ABRT, TRAP, SYS, ILL, SEGV, BUS, FPE) or "terminate" (STKFLT,
// USR1, …) would end the sandbox (code review H2). The kernel does not deliver
// SIGKILL and SIGSTOP to PID 1 inside the namespace. The orchestrator removes
// containers with SIGKILL from outside.
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

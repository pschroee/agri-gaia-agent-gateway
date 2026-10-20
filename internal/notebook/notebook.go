// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package notebook runs the agent's own code in the chat's notebook server in JupyterHub (own code
// C-E3). The server belongs to the user of the chat (their JupyterHub, their files); the gateway
// starts it through the hub API, copies the approved files in with docker exec and reads state and
// results back the same way. Every call the agent makes goes through the notebook paths under
// platform.NotebookPrefix, which the manager answers with this service (RegisterVirtual, G1.5), so
// delegation, effect class, approval, plan and log apply without a special way.
package notebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// Docker is what the runner needs from the container runtime: a command in the notebook server's
// container as the notebook user, and the containers that carry the gateway's labels.
type Docker interface {
	ExecAs(ctx context.Context, id, user string, cmd []string, stdin io.Reader) ([]byte, int, error)
	FindByLabels(ctx context.Context, labels map[string]string) ([]string, error)
}

// Config is the installed setup (AGW_JUPYTERHUB_*, AGW_NOTEBOOK_* in internal/config).
type Config struct {
	// APIURL is the hub API (…/hub/api). Empty: the service is not built and the notebook tools
	// answer "not configured on this instance" (the manager registers no doer then).
	APIURL    string
	Token     string
	PublicURL string // what the user opens, for the link of a run
	// Prefix of the server name in the hub. The spawner of the instance hardens only servers whose
	// name carries it, so the gateway never starts one without it.
	Prefix        string
	TimeoutMaxMin int
	DatasetMaxMB  int
	IdleMin       int
	// GPU: the spawner gives the servers a GPU (AGW_NOTEBOOK_GPUS); only stated to the user and the agent.
	GPU bool
}

// Notebooks is the notebook service of the gateway.
type Notebooks struct {
	cfg    Config
	hub    *Hub
	docker Docker
	store  *store.Store
	plat   *platform.Client
	// LogCall records a platform call the gateway made for a chat (the manager's LogCallBy). Nil:
	// nothing is logged; the calls the agent makes are logged by the socket anyway.
	LogCall func(ctx context.Context, chatID, detail, result string)
	// now is the clock (tests replace it).
	now func() time.Time
	// locks holds one *sync.Mutex per chat (see chatLock).
	locks sync.Map
}

// chatLock returns the mutex of a chat's notebook server. It excludes a run that is starting from
// the reaper that stops the idle server, so the reaper never stops a server under a starting run.
// It holds because the gateway is one process; a second gateway process would need a database lock
// (pg_advisory_xact_lock in the reaper plus the re-check) instead.
func (n *Notebooks) chatLock(chatID string) *sync.Mutex {
	m, _ := n.locks.LoadOrStore(chatID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// notebookUID is the user of the notebook image (jovyan) and the group of the gateway's labels.
const (
	notebookUID = "1000:100"
	// workDir is the home of the notebook user; runs and datasets live below it.
	workDir = "/home/jovyan/work"
)

// New returns the service; cfg.APIURL must be set (main wires it only then).
func New(cfg Config, plat *platform.Client, st *store.Store, docker Docker) *Notebooks {
	return &Notebooks{cfg: cfg, hub: NewHub(cfg.APIURL, cfg.Token, nil), docker: docker, store: st, plat: plat, now: time.Now}
}

// Server names this chat's notebook server: the configured prefix and the first eight hex digits of
// the chat id (without its hyphens). The name is short enough for a container name and the agent
// never sets it.
func (n *Notebooks) Server(chatID string) string {
	hex := strings.ReplaceAll(chatID, "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	prefix := n.cfg.Prefix
	if prefix == "" {
		prefix = "agw"
	}
	return prefix + "-" + hex
}

// Link is the address the user opens for a run (the JupyterLab tree of its run directory).
func (n *Notebooks) Link(user, server, runID string) string {
	base := strings.TrimRight(n.cfg.PublicURL, "/")
	if base == "" || user == "" {
		return ""
	}
	return base + "/user/" + user + "/" + server + "/lab/tree/work/runs/" + runID
}

// withoutNotebook is what the agent should do when the capability is missing (G1.13).
const withoutNotebook = "train small models in the sandbox (numpy, pandas, no torch) or use a platform training template"

// unavailable is the answer of a notebook tool without the capability (G1.13).
func unavailable(reason string) platform.Result {
	return platform.Result{Status: platform.StatusUnavailable, Message: platform.UnavailableText("notebook", reason, withoutNotebook)}
}

// Do answers one of the notebook paths (the manager's virtual doer, G1.5). A missing capability is
// an answer with status unavailable, never an error the agent should retry (G1.13).
func (n *Notebooks) Do(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	if n == nil || n.hub == nil || n.cfg.APIURL == "" {
		return unavailable("it is not configured on this instance"), nil
	}
	user, err := n.hubUser(ctx, chatID)
	if err != nil {
		return unavailable("the user of this chat cannot be named for JupyterHub (" + err.Error() + ")"), nil
	}
	rel := strings.TrimPrefix(req.Path, platform.NotebookPrefix)
	server := n.Server(chatID)
	switch {
	case req.Method == http.MethodGet && rel == "":
		return n.serverStatus(ctx, chatID, user, server)
	case req.Method == http.MethodGet && rel == "/runs":
		return n.runsResult(ctx, chatID)
	case req.Method == http.MethodPost && rel == "/runs":
		return n.runResult(ctx, chatID, user, server, req)
	case req.Method == http.MethodPost && rel == "/stop":
		return n.stopResult(ctx, chatID, user, server)
	}
	if m := runPathRe.FindStringSubmatch(rel); m != nil && req.Method == http.MethodGet {
		switch m[2] {
		case "":
			return n.statusResult(ctx, chatID, m[1])
		case "/log":
			return n.logResult(ctx, chatID, m[1], req)
		case "/file":
			return n.fetchResult(ctx, chatID, m[1], req)
		}
	}
	return platform.Result{Status: "error", Message: "unknown notebook call " + req.String()}, nil
}

// runPathRe is the part of a notebook path after the prefix that names one run.
var runPathRe = regexp.MustCompile(`^/runs/(r[0-9]{14}-[0-9a-f]{4})(/log|/file)?$`)

// hubUser names the user whose JupyterHub account the chat works with: the preferred_username of
// the token the chat uses for platform calls (G1.3), not chats.owner, which holds the subject.
func (n *Notebooks) hubUser(ctx context.Context, chatID string) (string, error) {
	if n.plat == nil {
		return "", platform.ErrNotConfigured
	}
	user, err := n.plat.ChatUser(ctx, chatID)
	if err != nil {
		return "", err
	}
	if !userRe.MatchString(user) {
		return "", errors.New("the name " + user + " does not fit JupyterHub's rules")
	}
	return user, nil
}

// hubProblem maps a failure of the hub to the answer of a tool: a hub that is not there and a user
// who never opened JupyterHub are both "unavailable" (G1.13), not errors to retry.
func unavailableHub(err error) platform.Result {
	if errors.Is(err, ErrNoHubUser) {
		return unavailable("the user of this chat has not opened JupyterHub yet; ask them to open it once in the browser")
	}
	if errors.Is(err, platform.ErrNotConfigured) {
		return unavailable("the platform binding of this gateway is off")
	}
	return unavailable("JupyterHub is not reachable (" + err.Error() + ")")
}

// StopChat stops the chat's notebook server (the trash does this; the files stay in the volume).
func (n *Notebooks) StopChat(ctx context.Context, chatID string) error {
	if n == nil || n.cfg.APIURL == "" {
		return nil
	}
	user, err := n.hubUser(ctx, chatID)
	if err != nil {
		return err
	}
	return n.hub.Stop(ctx, user, n.Server(chatID))
}

// log records a platform call the gateway made on its own (the dataset archives of a run). The
// calls the agent itself makes are logged by the socket; this one is not, so it is logged here.
func (n *Notebooks) log(ctx context.Context, chatID, detail, result string) {
	if n.LogCall == nil {
		return
	}
	n.LogCall(ctx, chatID, detail, result)
}

// jsonResult is the answer of a notebook tool: the JSON of one value and the link for the user.
func jsonResult(v any) (platform.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return platform.Result{Status: "error", Message: "notebook answer not writable: " + err.Error()}, nil
	}
	return platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: string(b)}, nil
}

// errorResult is the answer of a notebook tool that cannot do what it was asked.
func errorResult(format string, args ...any) (platform.Result, error) {
	return platform.Result{Status: "error", Message: fmt.Sprintf(format, args...)}, nil
}

// Shape is what a notebook server of this instance gets: the spawner's limits and, if configured, a GPU.
func (n *Notebooks) Shape() platform.NotebookShape {
	s := platform.DefaultNotebookShape
	s.GPU = n.cfg.GPU
	return s
}

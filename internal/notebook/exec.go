// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// The gateway runs everything in the notebook server with docker exec as the notebook user
// (1000:100), not through a Jupyter kernel (decision in the plan: exit code and log are reliable,
// no websocket, the gateway already has the docker socket). The container is found by the labels of
// its own spawner, never by name alone.

// labels name the container of a chat's notebook server. The spawner of the instance sets them
// (plan E2); the gateway only reads them.
const (
	labelNotebook = "agw.notebook"
	labelUser     = "agw.user"
	labelServer   = "agw.server"
)

// errNoContainer: the chat's notebook server is not running (stopped by the reaper, never started).
var errNoContainer = fmt.Errorf("the notebook server of this chat is not running")

// container returns the id of the running container of this server. Exactly one container must
// carry the labels; none or several are an error (the labels belong to the instance's spawner).
func (n *Notebooks) container(ctx context.Context, user, server string) (string, error) {
	ids, err := n.docker.FindByLabels(ctx, map[string]string{labelNotebook: "1", labelUser: user, labelServer: server})
	if err != nil {
		return "", fmt.Errorf("looking for the notebook server: %w", err)
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", errNoContainer
	}
	return "", fmt.Errorf("the notebook server of this chat is ambiguous: %d containers carry its labels", len(ids))
}

// exec runs a command in the container as the notebook user. A code below zero means the command
// never ran (a failure of docker); otherwise code is what the command returned and err carries its
// stderr, so a caller that expects a non-zero code (test -e) can tell the two apart.
func (n *Notebooks) exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (out []byte, code int, err error) {
	out, code, err = n.docker.ExecAs(ctx, id, notebookUID, cmd, stdin)
	if code < 0 {
		return out, code, fmt.Errorf("running %s in the notebook server: %w", strings.Join(cmd, " "), err)
	}
	return out, code, err
}

// mustExec runs a command that has to succeed.
func (n *Notebooks) mustExec(ctx context.Context, id string, cmd []string, stdin io.Reader) ([]byte, error) {
	out, code, err := n.exec(ctx, id, cmd, stdin)
	if code != 0 {
		return out, fmt.Errorf("in the notebook server: %s answers %d: %v", strings.Join(cmd, " "), code, err)
	}
	return out, nil
}

// exists reports whether a path exists in the container (the marker files of a run).
func (n *Notebooks) exists(ctx context.Context, id, path string) (bool, error) {
	_, code, err := n.exec(ctx, id, []string{"test", "-e", path}, nil)
	if err != nil && code <= 0 {
		return false, err
	}
	return code == 0, nil
}

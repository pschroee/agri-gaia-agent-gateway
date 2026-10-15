// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FileData is a file of the platform that does not go to the agent but into the execution sandbox
// (own code C-E3: notebook_fetch pulls a result of a run into the workspace, where upload_model
// then finds it). Dir is one plain path segment below /workspace/notebook, Name one file name; the
// socket writes it, the agent cannot name the destination itself.
type FileData struct {
	Dir  string
	Name string
	Data []byte
}

// DefaultMaxStream limits what Stream hands out when the configuration says nothing (2 GiB, the
// default of AGW_NOTEBOOK_DATASET_MAX_MB).
const DefaultMaxStream = 2 << 30

// Stream performs a call and hands back its response body unread (dataset archives for a notebook
// run, own code C-E3): the gateway copies it on instead of holding it in memory. The call is
// checked like every other one (Normalize including the blockers) and gets its token the same way
// as send. A status outside 2xx is an error, not a Result — the caller has no place to hand a body
// to. size is what the platform declares (-1 when it says nothing); the caller counts the bytes it
// actually reads and closes the body.
func (c *Client) Stream(ctx context.Context, chatID string, r Request) (body io.ReadCloser, size int64, err error) {
	if c == nil {
		return nil, 0, ErrNotConfigured
	}
	r, err = Normalize(r)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.send(ctx, chatID, r, false)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		resp, err = c.send(ctx, chatID, r, true)
	}
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, 0, fmt.Errorf("%s: the platform answers %d: %s", r.String(), resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	limit := c.cfg.MaxStream
	if limit <= 0 {
		limit = DefaultMaxStream
	}
	if resp.ContentLength > limit {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("%s: the platform announces %d bytes, more than this gateway copies at once (%d MB)",
			r.String(), resp.ContentLength, limit>>20)
	}
	return resp.Body, resp.ContentLength, nil
}

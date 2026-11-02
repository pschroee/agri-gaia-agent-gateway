// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package agwclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
)

// ExportedTable is one streamed table of the experiment export (X5.7).
type ExportedTable struct {
	Body    io.ReadCloser
	Columns []string
	Version string
}

// ExportTable opens one table of GET /api/exp/export. The caller closes Body.
func (c *Client) ExportTable(ctx context.Context, plan, table string) (*ExportedTable, error) {
	q := url.Values{"plan": {plan}, "table": {table}}
	resp, err := c.request(ctx, "GET", "/api/exp/export?"+q.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	out := &ExportedTable{Body: resp.Body, Version: resp.Header.Get("X-Agw-Gateway-Version")}
	if raw := resp.Header.Get("X-Agw-Columns"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &out.Columns); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("columns header unreadable: %w", err)
		}
	}
	return out, nil
}

// ExpWorkspace streams the archive of one experiment run's working directory (GET /api/exp/workspace) into w and
// returns the SHA-256 the gateway recorded for it. A chat without an archive gives an *APIError with status 404 whose
// message says why. The caller compares the checksum with what it received.
func (c *Client) ExpWorkspace(ctx context.Context, chat string, w io.Writer) (string, error) {
	resp, err := c.request(ctx, "GET", "/api/exp/workspace?"+url.Values{"chat": {chat}}.Encode(), nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		return "", err
	}
	return resp.Header.Get("X-Agw-Workspace-SHA256"), nil
}

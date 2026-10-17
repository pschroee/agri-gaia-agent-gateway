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

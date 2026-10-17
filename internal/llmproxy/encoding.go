// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// decompress turns a gzip-encoded upstream body into a plain stream and drops the headers that describe the
// compression (issue #151). The proxy meters every response while it passes it on; a compressed body would be
// unreadable there and the model call would be recorded without tokens, while pi (which decodes the body)
// looks fine. The sandbox receives the plain body; that is the same content, only without the transfer
// encoding.
func decompress(resp *http.Response) error {
	if !strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		return nil
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = &gzipBody{zr: zr, body: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

// gzipBody reads the decompressed stream and closes the original body with it.
type gzipBody struct {
	zr   *gzip.Reader
	body io.Closer
}

func (b *gzipBody) Read(p []byte) (int, error) { return b.zr.Read(p) }

func (b *gzipBody) Close() error {
	err := b.zr.Close()
	if cerr := b.body.Close(); err == nil {
		err = cerr
	}
	return err
}

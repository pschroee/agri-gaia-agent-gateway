// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeArtifacts struct{ data []byte }

func (f *fakeArtifacts) OpenArtifact(_ context.Context, _, _, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

// callDownload runs the download handler for one stored file; query is e.g. "?inline=1".
func callDownload(name, query string, data []byte) *httptest.ResponseRecorder {
	s := &Server{Artifacts: &fakeArtifacts{data: data}}
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/api/chats/c1/artifacts/"+name+query, nil)
	r.SetPathValue("id", "c1")
	r.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.download(w, r)
	return w
}

// Inline previews: the kind comes from the bytes, every response carries nosniff and no-store, and HTML or SVG
// never leave as a document.
func TestDownloadInline(t *testing.T) {
	const imgCSP = "sandbox; default-src 'none'"
	const pdfCSP = "default-src 'none'; object-src 'self'; plugin-types application/pdf"
	png := []byte("\x89PNG\r\n\x1a\nrest")
	cases := []struct {
		name, file, ct, csp string
		data                []byte
	}{
		{"plot.png", "plot.png", "image/png", imgCSP, png},
		{"photo.jpg", "photo.jpg", "image/jpeg", imgCSP, []byte("\xff\xd8\xff\xe0JFIF")},
		{"doc.pdf", "doc.pdf", "application/pdf", pdfCSP, []byte("%PDF-1.4\n")},
		{"png-as-txt.txt", "picture.txt", "image/png", imgCSP, png},
		{"table.csv", "table.csv", "text/plain; charset=utf-8", imgCSP, []byte("a,b\n1,2\n")},
		{"out.json", "out.json", "text/plain; charset=utf-8", imgCSP, []byte(`{"a":1}`)},
		{"page.html", "page.html", "text/plain; charset=utf-8", imgCSP, []byte("<script>alert(1)</script>")},
		{"logo.svg", "logo.svg", "text/plain; charset=utf-8", imgCSP, []byte(`<svg onload="alert(1)"></svg>`)},
	}
	for _, c := range cases {
		w := callDownload(c.file, "?inline=1", c.data)
		hd := w.Header()
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), c.data) {
			t.Fatalf("%s: %d %q", c.name, w.Code, w.Body.String())
		}
		if hd.Get("Content-Type") != c.ct || hd.Get("Content-Security-Policy") != c.csp {
			t.Fatalf("%s: type %q, csp %q", c.name, hd.Get("Content-Type"), hd.Get("Content-Security-Policy"))
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Cache-Control") != "private, no-store" ||
			!strings.HasPrefix(hd.Get("Content-Disposition"), "inline") {
			t.Fatalf("%s: headers %v", c.name, hd)
		}
		if strings.Contains(hd.Get("Content-Type"), "html") || strings.Contains(hd.Get("Content-Type"), "svg") {
			t.Fatalf("%s: rendered as markup: %q", c.name, hd.Get("Content-Type"))
		}
	}
	// Binary data that is no preview kind answers 415; the body is text.
	if w := callDownload("blob.bin", "?inline=1", []byte{0x00, 0xfe, 0xff}); w.Code != 415 ||
		!strings.Contains(w.Body.String(), "no preview for this file type") {
		t.Fatalf("binary: %d %q", w.Code, w.Body.String())
	}
	// Without inline the download stays an attachment, as before.
	w := callDownload("page.html", "", []byte("<p>x</p>"))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Security-Policy") != "" {
		t.Fatalf("attachment: %d %v", w.Code, w.Header())
	}
}

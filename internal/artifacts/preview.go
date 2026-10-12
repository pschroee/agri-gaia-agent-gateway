// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package artifacts

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// PreviewHeadSize is how many leading bytes PreviewKind looks at.
const PreviewHeadSize = 512

// Preview kinds of a file (see PreviewKind).
const (
	PreviewImage = "image"
	PreviewPDF   = "pdf"
	PreviewTable = "table"
	PreviewText  = "text"
	PreviewNone  = "none"
)

// previewTextExts are shown as plain text when their content is valid UTF-8.
var previewTextExts = map[string]bool{
	"txt": true, "md": true, "json": true, "jsonl": true, "log": true, "yaml": true, "yml": true,
	"xml": true, "py": true, "ts": true, "js": true, "sh": true, "ipynb": true,
}

// markupExts are markup that is never rendered: always shown as plain text (source), never as HTML or SVG.
var markupExts = map[string]bool{"html": true, "htm": true, "svg": true}

// PreviewKind decides how a file can be previewed. name is the file name, head its first
// PreviewHeadSize bytes. Image and PDF are decided by magic bytes, never by the extension. HTML, HTM and SVG
// are always text (source) and never rendered. The result is one of PreviewImage, PreviewPDF, PreviewTable,
// PreviewText or PreviewNone.
func PreviewKind(name string, head []byte) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if markupExts[ext] {
		if isUTF8Head(head) {
			return PreviewText
		}
		return PreviewNone
	}
	if ImageType(head) != "" {
		return PreviewImage
	}
	if bytes.HasPrefix(head, []byte("%PDF-")) {
		return PreviewPDF
	}
	if !isUTF8Head(head) {
		return PreviewNone
	}
	switch {
	case ext == "csv" || ext == "tsv":
		return PreviewTable
	case previewTextExts[ext]:
		return PreviewText
	}
	return PreviewNone
}

// ImageType returns the MIME type of a PNG, JPEG, GIF or WebP file by its magic bytes, otherwise "".
func ImageType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// isUTF8Head reports whether head is valid UTF-8 without NUL bytes. A multi-byte character cut off at the
// end of head (head is only the start of the file) does not count against it.
func isUTF8Head(head []byte) bool {
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	for i := 0; i < len(head); {
		r, size := utf8.DecodeRune(head[i:])
		if r == utf8.RuneError && size == 1 {
			return !utf8.FullRune(head[i:]) // only an incomplete character at the very end is allowed
		}
		i += size
	}
	return true
}

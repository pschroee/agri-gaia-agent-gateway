// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package artifacts

import "testing"

func TestPreviewKind(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpeg := []byte("\xff\xd8\xff\xe0JFIF")
	gif := []byte("GIF89a....")
	webp := []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")
	pdf := []byte("%PDF-1.4\n%....")
	binary := []byte{0x00, 0x01, 0xfe, 0xff, 0x00, 0x7f}
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"plot.png", png, PreviewImage},
		{"photo.jpg", jpeg, PreviewImage},
		{"anim.gif", gif, PreviewImage},
		{"pic.webp", webp, PreviewImage},
		{"doc.pdf", pdf, PreviewPDF},
		{"notes.txt", pdf, PreviewPDF},
		{"plot.png", []byte("just text, not a picture\n"), PreviewNone},
		{"data.csv", []byte("a,b\n1,2\n"), PreviewTable},
		{"data.TSV", []byte("a\tb\n1\t2\n"), PreviewTable},
		{"data.csv", png, PreviewImage},
		{"data.csv", binary, PreviewNone},
		{"readme.md", []byte("# Titel\näöü\n"), PreviewText},
		{"out.json", []byte(`{"a": 1}`), PreviewText},
		{"run.log", []byte("ok\n"), PreviewText},
		{"blob.bin", []byte("plain"), PreviewNone},
		{"page.html", []byte("<script>alert(1)</script>"), PreviewText},
		{"page.htm", []byte("<html></html>"), PreviewText},
		{"logo.svg", []byte(`<svg onload="alert(1)"><script>alert(1)</script></svg>`), PreviewText},
		{"logo.SVG", png, PreviewNone},
		{"page.html", binary, PreviewNone},
		{"bad.txt", []byte{'o', 'k', 0xc3}, PreviewText},
		{"bad.txt", []byte{'o', 'k', 0xff, 'x'}, PreviewNone},
		{"bad.csv", []byte{'a', ',', 0xff}, PreviewNone},
		{"empty.txt", []byte{}, PreviewText},
		{"noext", []byte("text"), PreviewNone},
	}
	for _, c := range cases {
		if got := PreviewKind(c.name, c.head); got != c.want {
			t.Errorf("PreviewKind(%q, %q) = %s, want %s", c.name, c.head, got, c.want)
		}
	}
}

func TestImageType(t *testing.T) {
	if ImageType([]byte("\x89PNG\r\n\x1a\n")) != "image/png" || ImageType([]byte("GIF87a")) != "image/gif" {
		t.Fatal("png or gif not recognised")
	}
	if ImageType([]byte("RIFF\x00\x00\x00\x00AVI ")) != "" || ImageType([]byte("<svg>")) != "" {
		t.Fatal("RIFF without WEBP or SVG taken for an image")
	}
}

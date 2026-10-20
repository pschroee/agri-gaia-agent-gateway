// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
)

// ListedFiles: up to this many files an approval lists one by one; more files are summarised and their full list
// is attached to the approval as a manifest (issue #164).
const ListedFiles = 10

// maxFolderLines limits the folder lines of the preview of a summarised upload.
const maxFolderLines = 50

// UploadEntry is one file of an upload: form field, path in the sandbox, size and SHA-256 of the bytes.
type UploadEntry struct {
	Field  string `json:"field"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// UploadFolder counts the files of one folder; Path is relative to the root of the manifest, "." is the root.
type UploadFolder struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// UploadExt counts the files of one extension ("(none)" for files without one).
type UploadExt struct {
	Ext   string `json:"ext"`
	Files int    `json:"files"`
}

// UploadManifest describes the files of one upload call. The approval carries it without Entries (the list of
// every file is fetched by the card on demand, issue #164); SHA256 is the SHA-256 over Lines() of all entries
// and binds the summary to exactly that list.
type UploadManifest struct {
	Files      int            `json:"files"`
	Bytes      int64          `json:"bytes"`
	Root       string         `json:"root"`
	Folders    []UploadFolder `json:"folders"`
	Extensions []UploadExt    `json:"extensions"`
	Annotation *UploadEntry   `json:"annotation,omitempty"`
	SHA256     string         `json:"manifest_sha256"`
	Entries    []UploadEntry  `json:"entries,omitempty"`
}

var errFilesChanged = errors.New("the files are not the ones the user approved; nothing was sent")

// Summary returns the manifest without the list of every file.
func (m UploadManifest) Summary() UploadManifest {
	m.Entries = nil
	return m
}

// Lines is the text the manifest hash is taken over: one line per file in the order they are sent, with tabs
// between the fields. Paths cannot contain tabs or line breaks (normalize and ManifestOf refuse control
// characters), so the lines are unambiguous.
func (m UploadManifest) Lines() string { return entryLines(m.Entries) }

func entryLines(entries []UploadEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\n", e.Field, e.Path, e.Bytes, e.SHA256)
	}
	return b.String()
}

// ManifestOf builds the manifest of the uploads of a request that have been read. The path of an entry is the
// sandbox path when Files names the uploads, otherwise the name of the upload.
func ManifestOf(r Request) (UploadManifest, error) {
	entries := make([]UploadEntry, len(r.Uploads))
	for i, u := range r.Uploads {
		p := u.Name
		if len(r.Files) == len(r.Uploads) {
			p = r.Files[i].Path
		}
		entries[i] = UploadEntry{Field: u.Field, Path: p, Bytes: int64(len(u.Data)), SHA256: u.SHA256}
	}
	return buildManifest(entries, first(r.Form["includes_annotation_file"]) == "true")
}

// buildManifest aggregates the entries. With annotationLast the last entry is the annotation file: it counts in
// the totals, the entries and the hash, but not in root, folders and extensions.
func buildManifest(entries []UploadEntry, annotationLast bool) (UploadManifest, error) {
	m := UploadManifest{Entries: entries, Folders: []UploadFolder{}, Extensions: []UploadExt{}}
	for _, e := range entries {
		if strings.IndexFunc(e.Path, unicode.IsControl) >= 0 {
			return UploadManifest{}, fmt.Errorf("file path %q invalid: control characters are not allowed", e.Path)
		}
		m.Files++
		m.Bytes += e.Bytes
	}
	body := entries
	if annotationLast && len(entries) > 0 {
		last := entries[len(entries)-1]
		m.Annotation = &last
		body = entries[:len(entries)-1]
	}
	m.Root = commonDir(body)
	folders := map[string]*UploadFolder{}
	exts := map[string]int{}
	for _, e := range body {
		rel := "."
		if d := path.Dir(e.Path); d != m.Root {
			rel = strings.TrimPrefix(strings.TrimPrefix(d, m.Root), "/")
		}
		f := folders[rel]
		if f == nil {
			f = &UploadFolder{Path: rel}
			folders[rel] = f
		}
		f.Files++
		f.Bytes += e.Bytes
		ext := strings.ToLower(path.Ext(e.Path))
		if ext == "" {
			ext = "(none)"
		}
		exts[ext]++
	}
	for _, f := range folders {
		m.Folders = append(m.Folders, *f)
	}
	sort.Slice(m.Folders, func(i, j int) bool { return m.Folders[i].Path < m.Folders[j].Path })
	for e, n := range exts {
		m.Extensions = append(m.Extensions, UploadExt{Ext: e, Files: n})
	}
	sort.Slice(m.Extensions, func(i, j int) bool {
		if m.Extensions[i].Files != m.Extensions[j].Files {
			return m.Extensions[i].Files > m.Extensions[j].Files
		}
		return m.Extensions[i].Ext < m.Extensions[j].Ext
	})
	sum := sha256.Sum256([]byte(m.Lines()))
	m.SHA256 = hex.EncodeToString(sum[:])
	return m, nil
}

// commonDir is the longest directory that contains all entries ("" without entries).
func commonDir(entries []UploadEntry) string {
	if len(entries) == 0 {
		return ""
	}
	common := strings.Split(path.Dir(entries[0].Path), "/")
	for _, e := range entries[1:] {
		parts := strings.Split(path.Dir(e.Path), "/")
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	dir := strings.Join(common, "/")
	if dir == "" && strings.HasPrefix(entries[0].Path, "/") {
		return "/"
	}
	return dir
}

// CheckManifest recomputes the manifest from the bytes about to be sent; the SHA-256 of every upload is taken
// again from its data, never from Upload.SHA256.
func CheckManifest(r Request, approved string) error {
	for i := range r.Uploads {
		sum := sha256.Sum256(r.Uploads[i].Data)
		if hex.EncodeToString(sum[:]) != r.Uploads[i].SHA256 {
			return errFilesChanged
		}
	}
	m, err := ManifestOf(r)
	if err != nil || approved == "" || m.SHA256 != approved {
		return errFilesChanged
	}
	return nil
}

// typesText lists the file types with their counts.
func (m UploadManifest) typesText() string {
	parts := make([]string, len(m.Extensions))
	for i, e := range m.Extensions {
		parts[i] = fmt.Sprintf("%s %d", e.Ext, e.Files)
	}
	return strings.Join(parts, ", ")
}

// describe is the text of the preview for an upload above ListedFiles.
func (m UploadManifest) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %d bytes in total, manifest sha256 %s\n", fileCount(m.Files), m.Bytes, m.SHA256)
	if len(m.Folders) > 0 {
		fmt.Fprintf(&b, "Folders under %s:\n", m.Root)
		for i, f := range m.Folders {
			if i == maxFolderLines {
				fmt.Fprintf(&b, "  … and %d more folders\n", len(m.Folders)-maxFolderLines)
				break
			}
			fmt.Fprintf(&b, "  %s: %s, %d bytes\n", f.Path, fileCount(f.Files), f.Bytes)
		}
	}
	if len(m.Extensions) > 0 {
		fmt.Fprintf(&b, "File types: %s\n", m.typesText())
	}
	if a := m.Annotation; a != nil {
		fmt.Fprintf(&b, "Annotation file: %s: %s  %d bytes  sha256 %s\n", a.Field, a.Path, a.Bytes, a.SHA256)
	}
	b.WriteString("The list of all files (path, size, SHA-256) is attached to the approval.")
	return b.String()
}

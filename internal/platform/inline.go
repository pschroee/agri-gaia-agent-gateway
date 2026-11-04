// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Files given in the call itself (BM12 #276). A tool that takes files of the sandbox (notebook_run, upload_dataset)
// also takes their content as arguments, so that a binding without a shell or file tools can start a run and upload
// files like the command line. The content is not a second path to the platform: the tool builds the same call it
// builds for sandbox files, the gateway writes the content into the sandbox just before it reads the files (sock,
// writeInline), and from there the delegation, the approval, the limits and the log see the same call.

// InlineDir is where the content given in a call lands in the sandbox, in a directory of its own per call.
const InlineDir = "/workspace/inline"

// InlineMaxBytes is the most content one call may carry (decoded, all files together).
const InlineMaxBytes = 4 << 20

// InlineMaxFiles is the most files one call may carry as content.
const InlineMaxFiles = 50

var inlineNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]{0,119}$`)

// InlineFile is one file of a call that is written into the sandbox at Path before the call is checked.
type InlineFile struct {
	Path string
	Data []byte
}

// Param descriptions of the content arguments (shared by the tools that take them).
const (
	contentsDesc = "files given as content instead of as sandbox paths: an object from file name to text (UTF-8), e.g. {\"main.py\": \"print(1)\"}. " +
		"File names are plain (letters, digits, _ . + -). At most 50 files and 4 MB together with contents_base64. " +
		"They count like files of the sandbox in every check and in the approval"
	contentsBase64Desc = "files given as base64 content (binary files such as images): an object from file name to base64 text. Limits as for contents"
)

// inlineEntries reads contents (text) and contents_base64 (binary) of a call: the files in the order of their names.
func inlineEntries(a Args) ([]InlineFile, error) {
	var out []InlineFile
	seen := map[string]bool{}
	var total int64
	for _, key := range []string{"contents", "contents_base64"} {
		raw, ok, err := a.stringMap(key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for name, v := range raw {
			if !inlineNameRe.MatchString(name) {
				return nil, fmt.Errorf("%s: file name %q invalid: plain name of letters, digits, _ . + -, at most 120 characters", key, name)
			}
			if seen[name] {
				return nil, fmt.Errorf("file %q is given twice (contents and contents_base64)", name)
			}
			seen[name] = true
			data := []byte(v)
			if key == "contents_base64" {
				d, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
				if err != nil {
					return nil, fmt.Errorf("contents_base64: %q is not valid base64", name)
				}
				data = d
			} else if !utf8.ValidString(v) {
				return nil, fmt.Errorf("contents: %q is not valid UTF-8 text (binary files go into contents_base64)", name)
			}
			total += int64(len(data))
			out = append(out, InlineFile{Path: name, Data: data})
		}
	}
	if len(out) > InlineMaxFiles {
		return nil, fmt.Errorf("at most %d files as content per call, %d given", InlineMaxFiles, len(out))
	}
	if total > InlineMaxBytes {
		return nil, fmt.Errorf("the content of the files is larger than %d MB; larger files come from a run (%s) and are used from the sandbox", InlineMaxBytes>>20, ToolRef("notebook_fetch"))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) == 0 {
		return nil, nil
	}
	// One directory per set of files: the path is a function of the content, so building the call twice (plan
	// step and call, retry) gives the same paths.
	h := sha256.New()
	for _, f := range out {
		h.Write([]byte(strconv.Itoa(len(f.Path)) + ":" + f.Path + "\x00" + strconv.Itoa(len(f.Data)) + ":"))
		h.Write(f.Data)
	}
	dir := InlineDir + "/" + hex.EncodeToString(h.Sum(nil))[:16]
	for i := range out {
		out[i].Path = dir + "/" + out[i].Path
	}
	return out, nil
}

// stringMap reads an object whose values are all strings.
func (a Args) stringMap(name string) (map[string]string, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return nil, false, nil
	}
	m, isObj := v.(map[string]any)
	if !isObj {
		return nil, false, fmt.Errorf("%s must be an object from file name to text", name)
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		s, isStr := x.(string)
		if !isStr {
			return nil, false, fmt.Errorf("%s: the value of %q must be a string", name, k)
		}
		out[k] = s
	}
	return out, true, nil
}

// bool reads a flag: true/false, or the strings "true"/"false" (the command line and the REST query pass text).
func (a Args) bool(name string) (bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return false, nil
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no", "":
			return false, nil
		}
	}
	return false, fmt.Errorf("%s must be true or false", name)
}

// --- notebook_fetch: show a result file instead of copying it ---

const (
	// ViewTextMax is the longest text a fetch shows in the answer (the limit of every tool answer).
	ViewTextMax = 64 << 10
	// ViewImageMax is the largest image a fetch shows.
	ViewImageMax = 4 << 20
)

// ImageData is an image in an answer.
type ImageData struct {
	MIME string
	Data []byte
}

// ViewFile turns the file of a fetch into what the answer shows: the text, or an image. The reason is set when
// the file cannot be shown; it names what to do instead.
func ViewFile(f *FileData) (body string, img *ImageData, why string) {
	sniff := f.Data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	switch mime := detectImage(f.Name, sniff); {
	case mime != "":
		if len(f.Data) > ViewImageMax {
			return "", nil, fmt.Sprintf("%s is an image of %d bytes; at most %d MB can be shown. Let the run write a smaller image", f.Name, len(f.Data), ViewImageMax>>20)
		}
		return fmt.Sprintf("image %s (%s, %d bytes) follows", f.Name, mime, len(f.Data)), &ImageData{MIME: mime, Data: f.Data}, ""
	case utf8.Valid(f.Data) && !strings.ContainsRune(string(f.Data), 0):
		if len(f.Data) > ViewTextMax {
			return "", nil, fmt.Sprintf("%s has %d bytes; at most %d KiB can be shown. Let the run write a shorter file (a summary, the first lines)", f.Name, len(f.Data), ViewTextMax>>10)
		}
		return string(f.Data), nil, ""
	}
	return "", nil, fmt.Sprintf("%s is neither text nor an image that can be shown; fetch it without inline and use it from the sandbox", f.Name)
}

func detectImage(name string, head []byte) string {
	switch {
	case len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "image/jpeg"
	case len(head) >= 6 && (string(head[:6]) == "GIF87a" || string(head[:6]) == "GIF89a"):
		return "image/gif"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

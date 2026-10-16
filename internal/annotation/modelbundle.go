// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
)

// A model of the platform is downloaded as a bundle when it holds more than one object: the backend
// packs the objects into an archive (backend, routers/models.py). A model that came out of a training
// carries its ONNX file and the training log, so the straight way from a trained model to a
// pre-annotation function leads through an archive (V2). The function itself needs a single ONNX
// file; the archive is opened here, and nothing but the one ONNX file is taken out of it.

// onnxFromBundle returns the model file of a download. raw is the downloaded body: when it is no
// archive, it is the file itself and is returned unchanged. An archive must hold exactly one *.onnx;
// none or several are refused with a message that names what the archive carries.
func onnxFromBundle(raw []byte, maxBytes int64) ([]byte, error) {
	if !bytes.HasPrefix(raw, zipMagic) {
		return raw, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("the download is an archive that cannot be read: %v", err)
	}
	var onnx []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.EqualFold(fileExt(f.Name), ".onnx") {
			onnx = append(onnx, f)
		}
	}
	switch len(onnx) {
	case 0:
		return nil, fmt.Errorf("the download is an archive without an ONNX file (%s); the function needs a model with a single ONNX file",
			bundleNames(zr.File))
	case 1:
		// the one ONNX file, read below
	default:
		return nil, fmt.Errorf("the download is an archive with %d ONNX files (%s); the function needs a model with a single ONNX file",
			len(onnx), bundleNames(onnx))
	}
	f := onnx[0]
	// The declared size is checked first, so that a large entry is refused before it is read; the
	// read itself is bounded as well, because the header of an archive is no promise.
	if int64(f.UncompressedSize64) > maxBytes {
		return nil, fmt.Errorf("the ONNX file %s in the archive is %d bytes; at most %d MiB (AGW_ANNOTATION_MODEL_MAX_MB)",
			f.Name, f.UncompressedSize64, maxBytes>>20)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("the ONNX file %s in the archive cannot be read: %v", f.Name, err)
	}
	defer rc.Close()
	raw, err = io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("the ONNX file %s in the archive cannot be read: %v", f.Name, err)
	}
	if int64(len(raw)) == 0 {
		return nil, fmt.Errorf("the ONNX file %s in the archive is empty", f.Name)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("the ONNX file %s in the archive is larger than %d MiB (AGW_ANNOTATION_MODEL_MAX_MB)",
			f.Name, maxBytes>>20)
	}
	return raw, nil
}

// bundleNames names the entries of an archive for a refusal; it stays short, because the message goes
// into the chat.
func bundleNames(files []*zip.File) string {
	var names []string
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		if len(names) == 4 {
			names = append(names, "…")
			break
		}
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		return "no file"
	}
	return strings.Join(names, ", ")
}

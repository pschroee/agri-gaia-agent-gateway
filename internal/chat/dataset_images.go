// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"agw/internal/platform"
)

// The platform's filecount of a dataset counts every stored file, the CVAT annotation (annotations.xml) among them;
// the approval of a training search must name images (issue #251, B4). The gateway therefore counts the images of the
// dataset's archive the way the split reads it: every file outside the annotation directory whose ending is an image
// ending.

// imageCounter counts the images of a dataset.
type imageCounter func(ctx context.Context, id int64) (int, error)

type imageCounterKey struct{}

// withImageCounter puts the counter in the context the approval's reads run in.
func withImageCounter(ctx context.Context, f imageCounter) context.Context {
	return context.WithValue(ctx, imageCounterKey{}, f)
}

func imageCounterFrom(ctx context.Context) imageCounter {
	f, _ := ctx.Value(imageCounterKey{}).(imageCounter)
	return f
}

// imageEndings are the endings that count as an image.
var imageEndings = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".bmp": true, ".gif": true, ".tif": true, ".tiff": true, ".webp": true}

// isImageName says whether a file of a dataset archive is an image.
func isImageName(name string) bool {
	return imageEndings[strings.ToLower(path.Ext(name))]
}

// countArchiveImages counts the images of a dataset archive; the annotation directory holds none.
func countArchiveImages(zr *zip.Reader) int {
	n := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(f.Name, "annotations/") || strings.Contains(f.Name, "/annotations/") {
			continue
		}
		if isImageName(f.Name) {
			n++
		}
	}
	return n
}

// imageCountTimeout and imageCountMaxBytes bound the count of one dataset; a dataset beyond them is named by its
// number of files instead.
const (
	imageCountTimeout  = 90 * time.Second
	imageCountMaxBytes = 256 << 20
)

// datasetImageCounter counts the images of a dataset from its archive, read with the chat's user token; each
// dataset is read once per counter.
func (m *Manager) datasetImageCounter(chatID string) imageCounter {
	var mu sync.Mutex
	memo := map[int64]int{}
	return func(ctx context.Context, id int64) (int, error) {
		mu.Lock()
		n, ok := memo[id]
		mu.Unlock()
		if ok {
			return n, nil
		}
		if m.opt.Platform == nil {
			return 0, platform.ErrNotConfigured
		}
		ctx, cancel := context.WithTimeout(ctx, imageCountTimeout)
		defer cancel()
		body, _, err := m.opt.Platform.Stream(ctx, chatID, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10) + "/download"})
		if err != nil {
			return 0, err
		}
		defer body.Close()
		f, err := os.CreateTemp("", "agw-count-*.zip")
		if err != nil {
			return 0, err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		size, err := io.Copy(f, io.LimitReader(body, imageCountMaxBytes+1))
		if err != nil {
			return 0, err
		}
		if size > imageCountMaxBytes {
			return 0, fmt.Errorf("the archive is larger than %d MB", imageCountMaxBytes>>20)
		}
		zr, err := zip.NewReader(f, size)
		if err != nil {
			return 0, err
		}
		n = countArchiveImages(zr)
		mu.Lock()
		memo[id] = n
		mu.Unlock()
		return n, nil
	}
}

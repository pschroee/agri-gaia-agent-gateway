// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/search"
)

func zipOf(t *testing.T, names ...string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, n := range names {
		f, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("x"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func datasetDoer(body string) platform.Doer {
	return func(ctx context.Context, r platform.Request) (platform.Result, error) {
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: body}, nil
	}
}

// #251 (B4): the platform's filecount includes annotations.xml; the approval names images only.
func TestSearchApprovalCountsImagesOnly(t *testing.T) {
	zr := zipOf(t, "a.png", "b.JPG", "sub/c.jpeg", "annotations/annotations.xml", "d/annotations/mask.png", "readme.txt")
	if n := countArchiveImages(zr); n != 3 {
		t.Fatalf("images in the archive: %d, want 3", n)
	}
	ds := datasetDoer(`{"name":"mnist","filecount":141,"annotation_labels":["0","1"]}`)
	// With a counter the images are named; the platform's number stays as files.
	ctx := withImageCounter(context.Background(), func(context.Context, int64) (int, error) { return 140, nil })
	n := readNamed(ctx, ds, 7)
	if n.Images != 140 || n.Files != 141 || n.Classes != 2 {
		t.Fatalf("named: %+v", n)
	}
	d := SearchApprovalData{Kind: searchApprovalProposal, Base: &SearchBase{ContainerID: 3, Dataset: &n}}
	if txt := searchSummary(d); !strings.Contains(txt, ", 140 images") || strings.Contains(txt, "141") {
		t.Fatalf("summary: %s", txt)
	}
	// A failed count says files, never images.
	ctx = withImageCounter(context.Background(), func(context.Context, int64) (int, error) { return 0, errors.New("download failed") })
	n = readNamed(ctx, ds, 7)
	d.Base.Dataset = &n
	if n.Images != 0 || n.Files != 141 {
		t.Fatalf("failed count: %+v", n)
	}
	if txt := searchSummary(d); !strings.Contains(txt, ", 141 files") || strings.Contains(txt, "images,") {
		t.Fatalf("summary after a failed count: %s", txt)
	}
}

// #251 (B8): a small validation set gets a hint with the Wilson bound; 28 images give at least 0.88.
func TestApprovalSmallValidationHint(t *testing.T) {
	fixed := map[string]json.RawMessage{"train-split": json.RawMessage(`0.8`)}
	if n := validationImages(140, fixed); n != 28 {
		t.Fatalf("validation images: %d, want 28", n)
	}
	if validationImages(140, map[string]json.RawMessage{}) != 0 || validationImages(0, fixed) != 0 {
		t.Fatal("unknown share or images must give 0 (the line is left out)")
	}
	d := SearchApprovalData{Kind: searchApprovalProposal, Base: &SearchBase{ContainerID: 3, ValImages: 28,
		Dataset: &SearchNamed{ID: 7, Images: 140}}}
	txt := searchSummary(d)
	want := "The validation value rests on about 28 images; 1.00 on 28 images means at least 0.88 with 95 % confidence."
	if !strings.Contains(txt, want) {
		t.Fatalf("hint missing:\n%s", txt)
	}
	d.Base.ValImages = 60
	if strings.Contains(searchSummary(d), "validation value rests on") {
		t.Fatal("no hint from 50 images on")
	}
	d.Base.ValImages = 0
	if strings.Contains(searchSummary(d), "validation value rests on") {
		t.Fatal("no hint without a size")
	}
}

// #251 (B9): the objective is named as the template's validation split, and the budget text names min_runs.
func TestSearchApprovalNamesValidation(t *testing.T) {
	d := SearchApprovalData{Kind: searchApprovalProposal, Objective: search.Objective{Metric: "val_acc", Mode: search.ModeMax}}
	if txt := searchSummary(d); !strings.Contains(txt, "val_acc (the template's validation split inside the training dataset") {
		t.Fatalf("objective text:\n%s", txt)
	}
	three, target := 3, 0.99
	if got := budgetText(search.Budget{Target: &target, MinRuns: &three}); !strings.Contains(got, "stop at ≥ 0.99 after at least 3 trials") {
		t.Fatalf("budget text: %s", got)
	}
}

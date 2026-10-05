package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agw/internal/platform"
)

func TestParseToolArgs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tc.json")
	_ = os.WriteFile(cfg, []byte(`{"epochs":1}`), 0o600)
	cases := []struct {
		tool  string
		args  []string
		stdin string
		want  string // built call
	}{
		{"datasets", []string{"--limit", "3"}, "", "GET /datasets?limit=3"},
		{"dataset", []string{"7"}, "", "GET /datasets/7"},
		{"train-options", []string{"Torchvision", "Mask R-CNN"}, "", "GET /train/config/Torchvision/Mask R-CNN"},
		{"training-logs", []string{"4", "--tail=50"}, "", "GET /train/containers/4/logs?tail=50"},
		{"create-training", []string{"Torchvision", "EfficientNet", "Classification", "2", "@" + cfg}, "", "POST /train/config"},
		{"create-training", []string{"--provider", "T", "--architecture", "E", "--category", "C", "--dataset-id", "2", "--train-config", "-"}, `{"epochs":2}`, "POST /train/config"},
		{"request", []string{"PATCH", "/datasets/3", "--body", `{"name":"x"}`, "--query", "a=b"}, "", "PATCH /datasets/3?a=b"},
	}
	for _, c := range cases {
		tool, _ := platform.Lookup(c.tool)
		in, err := parseToolArgs(tool, c.args, strings.NewReader(c.stdin))
		if err != nil {
			t.Fatalf("%s %v: %v", c.tool, c.args, err)
		}
		raw, _ := json.Marshal(in)
		r, err := tool.Build(raw)
		if err != nil || r.String() != c.want {
			t.Fatalf("%s %v: %q %v (arguments %s)", c.tool, c.args, r.String(), err, raw)
		}
	}
	// Uploads: file list at the end, relative paths become absolute, lists also as a repeated option.
	wd, _ := os.Getwd()
	tool, _ := platform.Lookup("upload-dataset")
	in, err := parseToolArgs(tool, []string{"piglets", "images", "a.png", "/workspace/b.png", "--annotation-labels", "0", "--annotation-labels", "1", "--annotation-file", "ann.xml"}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	r, err := tool.Build(raw)
	if err != nil || r.String() != "POST /datasets (multipart, 3 files)" || r.Files[0].Path != filepath.Join(wd, "a.png") || r.Files[2].Path != filepath.Join(wd, "ann.xml") ||
		strings.Join(r.Form["annotation_labels"], ",") != "0,1" || r.Form["includes_annotation_file"][0] != "true" {
		t.Fatalf("upload-dataset: %+v %v", r, err)
	}
	bad := [][]string{{"dataset"}, {"dataset", "1", "2"}, {"datasets", "--doesnotexist", "1"}, {"create-training", "T", "E", "C", "2", "{broken"}, {"request", "GET", "/x", "--query", "noEquals"}}
	for _, b := range bad {
		tool, _ := platform.Lookup(b[0])
		if _, err := parseToolArgs(tool, b[1:], strings.NewReader("")); err == nil {
			t.Errorf("expected an error: %v", b)
		}
	}
}

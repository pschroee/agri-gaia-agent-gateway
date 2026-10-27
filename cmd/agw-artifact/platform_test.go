package main

import (
	"encoding/json"
	"fmt"
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

// #198: the command index of the system note is the help text, line by line.
func TestCLIIndexMatchesHelp(t *testing.T) {
	var help strings.Builder
	platformUsage(&help)
	for _, line := range strings.Split(strings.TrimRight(platform.CLIIndex(), "\n"), "\n") {
		line = strings.TrimSuffix(line, " (approval)")
		if !strings.Contains(help.String(), "  "+line+"\n") {
			t.Errorf("index line %q is not a line of agw-platform --help", line)
		}
	}
}

// Before a call that may wait the CLI says what the gateway does (issue #225): a plan is checked first, another
// writing call is checked and confirmed where needed, a reading call says nothing.
func TestPlatformWaitLine(t *testing.T) {
	plan, _ := platform.Lookup("propose-plan")
	if l := waitLine(plan, map[string]any{}); !strings.Contains(l, "Proposing the plan") || !strings.Contains(l, "checks it first") {
		t.Errorf("plan: %q", l)
	}
	req, _ := platform.Lookup("request")
	if l := waitLine(req, map[string]any{"method": "PATCH"}); !strings.HasPrefix(l, "Writing call. The gateway checks it") {
		t.Errorf("writing call: %q", l)
	}
	if l := waitLine(req, map[string]any{"method": "GET"}); l != "" {
		t.Errorf("reading call: %q", l)
	}
}

func parseUpload(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	tool, _ := platform.Lookup("upload-dataset")
	return parseToolArgs(tool, args, strings.NewReader(""))
}

// K8 B1: a comma list for an array option without a path.
func TestParseArrayCommaList(t *testing.T) {
	in, err := parseUpload(t, "d", "x", "a.png", "--annotation_labels", "0,1", "--annotation_labels", "2")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmtAny(in["annotation_labels"]); got != "0,1,2" {
		t.Fatalf("labels %q", got)
	}
	if got := fmtAny(in["files"]); strings.Contains(got, ",") {
		t.Fatalf("files must stay a list of paths: %q", got)
	}
}

// K8 B1: a JSON array keeps class names with a comma.
func TestParseArrayJSON(t *testing.T) {
	in, err := parseUpload(t, "d", "x", "a.png", "--annotation_labels", `["a,b","c"]`)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := in["annotation_labels"].([]any)
	if len(l) != 2 || l[0] != "a,b" || l[1] != "c" {
		t.Fatalf("labels %v", in["annotation_labels"])
	}
	if _, err := parseUpload(t, "d", "x", "a.png", "--annotation_labels", `["a"`); err == nil {
		t.Fatal("broken JSON array must fail")
	}
}

// K8 B1: a second value after an array option is not a file; the message says how to give a list.
func TestHintAfterArrayOption(t *testing.T) {
	_, err := parseUpload(t, "d", "x", "--annotation_labels", "0", "1", "a.png")
	if err == nil || !strings.Contains(err.Error(), "--annotation_labels takes one value per option: repeat it (--annotation_labels 0 --annotation_labels 1) or give a list (0,1)") {
		t.Fatalf("hint missing: %v", err)
	}
}

func fmtAny(v any) string {
	l, _ := v.([]any)
	var s []string
	for _, x := range l {
		s = append(s, fmt.Sprint(x))
	}
	return strings.Join(s, ",")
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeLockMaterial writes a small material directory and returns its file map.
func writeLockMaterial(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"plan.yaml":                "purpose: main\nk: 3\n",
		"aufgaben/list.yaml":       "id: list\n",
		"fixtures/ds.yaml":         "id: ds\n",
		"codierschema/schema.yaml": "codes: []\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := MaterialFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, sums
}

func TestMaterialFilesAndCheckFiles(t *testing.T) {
	dir, files := writeLockMaterial(t)
	if len(files) != 4 {
		t.Fatalf("files: %v", files)
	}
	lock := &Lock{Files: files}
	if problems := lock.CheckFiles(dir); len(problems) != 0 {
		t.Fatalf("unchanged material: %v", problems)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.yaml"), []byte("purpose: main\nk: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems := lock.CheckFiles(dir)
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "plan.yaml differs") {
		t.Fatalf("changed file: %v", problems)
	}
}

func TestCheckFilesReportsMissingAndUnsafePaths(t *testing.T) {
	dir, files := writeLockMaterial(t)
	files["does-not-exist.yaml"] = strings.Repeat("0", 64)
	files["../outside.yaml"] = strings.Repeat("0", 64)
	lock := &Lock{Files: files}
	problems := lock.CheckFiles(dir)
	if len(problems) != 2 {
		t.Fatalf("problems: %v", problems)
	}
	joined := problems[0].String() + "\n" + problems[1].String()
	if !strings.Contains(joined, "does-not-exist.yaml") || !strings.Contains(joined, "outside.yaml") {
		t.Fatalf("findings: %s", joined)
	}
}

func testLock() *Lock {
	return &Lock{
		GatewayCommit: "abc123", GatewayDescribe: "abc123",
		ImagePi: "sha256:pi", ImageExec: "sha256:exec",
		CatalogSHA256: "cat", SkillsSHA256: "skills", InstructionSHA256: "instr", APIIndexSHA256: "api",
	}
}

func TestCompareEnvironmentMatches(t *testing.T) {
	env := Environment{
		GatewayCommit: "abc123", GatewayDescribe: "abc123",
		CatalogSHA256: "cat", SkillsSHA256: "skills", InstructionSHA256: "instr", APIIndexSHA256: "api",
		ImagesPi: []string{"sha256:pi"}, ImagesExec: []string{"sha256:exec"},
	}
	if problems := CompareEnvironment(testLock(), env); len(problems) != 0 {
		t.Fatalf("matching environment: %v", problems)
	}
}

func TestCompareEnvironmentReportsEachDeviation(t *testing.T) {
	env := Environment{
		GatewayCommit: "other", GatewayDescribe: "abc123",
		CatalogSHA256: "cat", SkillsSHA256: "skills2", InstructionSHA256: "instr3",
		ImagesPi: []string{"sha256:pi2"}, ImagesExec: []string{"sha256:exec"},
	}
	problems := CompareEnvironment(testLock(), env)
	joined := ""
	for _, p := range problems {
		joined += p.String() + "\n"
	}
	for _, want := range []string{"gateway_commit", "skills_sha256", "instruction_sha256", "image_pi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "image_exec") {
		t.Errorf("image_exec matches and must not be reported:\n%s", joined)
	}
}

// #237: the lock compares the inference runtime when it has one; an older lock without the value does not.
func TestCompareEnvironmentInferenceRuntime(t *testing.T) {
	env := Environment{
		GatewayCommit: "abc123", GatewayDescribe: "abc123",
		CatalogSHA256: "cat", SkillsSHA256: "skills", InstructionSHA256: "instr", APIIndexSHA256: "api",
		ImagesPi: []string{"sha256:pi"}, ImagesExec: []string{"sha256:exec"}, InferenceRuntime: "triton",
	}
	lock := testLock()
	lock.InferenceRuntime = "triton"
	if problems := CompareEnvironment(lock, env); len(problems) != 0 {
		t.Fatalf("same runtime: %v", problems)
	}
	env.InferenceRuntime = "ovms"
	problems := CompareEnvironment(lock, env)
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "inference_runtime") {
		t.Fatalf("other runtime: %v", problems)
	}
	if problems := CompareEnvironment(testLock(), env); len(problems) != 0 {
		t.Fatalf("lock without the value: %v", problems)
	}
}

func TestCompareEnvironmentNeedsWarmSlots(t *testing.T) {
	env := Environment{GatewayCommit: "abc123", GatewayDescribe: "abc123", CatalogSHA256: "cat",
		SkillsSHA256: "skills", InstructionSHA256: "instr", APIIndexSHA256: "api"}
	problems := CompareEnvironment(testLock(), env)
	if len(problems) != 2 {
		t.Fatalf("problems: %v", problems)
	}
	for _, p := range problems {
		if !strings.Contains(p.String(), "no warm experiment slot") {
			t.Errorf("unexpected finding: %s", p)
		}
	}
}

func TestCompareSlot(t *testing.T) {
	lock := testLock()
	if problems := CompareSlot(lock, EnvironmentSlot{ID: "s1", ImagePi: "sha256:pi", ImageExec: "sha256:exec"}); len(problems) != 0 {
		t.Fatalf("matching slot: %v", problems)
	}
	problems := CompareSlot(lock, EnvironmentSlot{ID: "s1", ImagePi: "sha256:old", ImageExec: "sha256:exec"})
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "image_pi") {
		t.Fatalf("mismatching slot: %v", problems)
	}
	if _, ok := (Environment{Slots: []EnvironmentSlot{{ID: "s1", ChatID: "c1"}}}).SlotForChat("c1"); !ok {
		t.Fatal("slot not found by chat")
	}
}

func TestTreeSHA256IsStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := TreeSHA256(dir)
	if err != nil || first == "" {
		t.Fatalf("tree: %q %v", first, err)
	}
	if again, _ := TreeSHA256(dir); again != first {
		t.Fatalf("not stable: %s vs %s", first, again)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := TreeSHA256(dir); changed == first {
		t.Fatal("content change did not change the checksum")
	}
	if missing, err := TreeSHA256(filepath.Join(dir, "nope")); err != nil || missing != "" {
		t.Fatalf("missing directory: %q %v", missing, err)
	}
}

// #198: the REST operation index is part of the frozen material; an empty or other value is reported.
func TestCompareEnvironmentAPIIndex(t *testing.T) {
	env := Environment{
		GatewayCommit: "abc123", GatewayDescribe: "abc123",
		CatalogSHA256: "cat", SkillsSHA256: "skills", InstructionSHA256: "instr", APIIndexSHA256: "other",
		ImagesPi: []string{"sha256:pi"}, ImagesExec: []string{"sha256:exec"},
	}
	problems := CompareEnvironment(testLock(), env)
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "api_index_sha256") {
		t.Fatalf("deviating index: %v", problems)
	}
	env.APIIndexSHA256 = ""
	problems = CompareEnvironment(testLock(), env)
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "api_index_sha256") {
		t.Fatalf("empty index: %v", problems)
	}
	lock := testLock()
	lock.APIIndexSHA256 = ""
	env.APIIndexSHA256 = "api"
	problems = CompareEnvironment(lock, env)
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "not frozen") {
		t.Fatalf("lock without index: %v", problems)
	}
}

// materialVector is the shared test vector of MaterialFiles; masterarbeit/versuch/test_einfrieren.py
// (test_material_files_gemeinsamer_vektor) holds the same files and the same expected checksums, so that
// Python and Go agree on which files the lock covers (BM17 K2).
var materialVector = struct {
	tracked   map[string]string // relative to the repository root, versioned in Git
	untracked map[string]string // present in the work tree, not versioned (one of them ignored)
	expected  map[string]string // MaterialFiles of the folder "versuch"
}{
	tracked: map[string]string{
		"outside.yaml":               "id: outside\n",
		"versuch/plan.yaml":          "purpose: main\nk: 3\n",
		"versuch/aufgaben/list.yaml": "id: list\n",
		"versuch/export/old.yaml":    "id: old\n",
		"versuch/plan.lock":          "{}\n",
		".gitignore":                 "node_modules/\n",
	},
	untracked: map[string]string{
		"versuch/node_modules/pkg/index.js": "x = 1\n",
		"versuch/scratch.yaml":              "id: scratch\n",
	},
	expected: map[string]string{
		"aufgaben/list.yaml": "1b424de9ee7a3d081e52053beedb4ddf9c93b535265c20a1b0e460199cd179a0",
		"plan.yaml":          "6cb3c2d22f8c43db226bbec32de36d8b7342e5df9ab16bd753c71688d58ca6f9",
	},
}

func writeVectorFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git %v: %v %s", args, err, out)
	}
}

// BM17 K2: inside a Git work tree an untracked or ignored file does not enter the lock.
func TestMaterialFilesCoverOnlyTrackedFiles(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	writeVectorFiles(t, root, materialVector.tracked)
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "x")
	writeVectorFiles(t, root, materialVector.untracked)

	got, err := MaterialFiles(filepath.Join(root, "versuch"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(materialVector.expected) {
		t.Fatalf("files: %v, want %v", got, materialVector.expected)
	}
	for name, want := range materialVector.expected {
		if got[name] != want {
			t.Fatalf("%s: %q, want %q (all: %v)", name, got[name], want, got)
		}
	}
	// the lock written from this set checks cleanly against the material, untracked files included
	lock := &Lock{Files: got}
	if problems := lock.CheckFiles(filepath.Join(root, "versuch")); len(problems) != 0 {
		t.Fatalf("check: %v", problems)
	}
}

// Without Git (the server copy from git archive) every file present counts, minus caches and results.
func TestMaterialFilesWithoutGitTakeEveryFile(t *testing.T) {
	root := t.TempDir()
	writeVectorFiles(t, root, materialVector.tracked)
	writeVectorFiles(t, root, materialVector.untracked)
	got, err := MaterialFiles(filepath.Join(root, "versuch"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"plan.yaml", "aufgaben/list.yaml", "scratch.yaml", "node_modules/pkg/index.js"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s missing from %v", want, got)
		}
	}
	for _, not := range []string{"export/old.yaml", "plan.lock"} {
		if _, ok := got[not]; ok {
			t.Fatalf("%s must stay out: %v", not, got)
		}
	}
}

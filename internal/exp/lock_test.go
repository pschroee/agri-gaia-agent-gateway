// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"os"
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

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// The frozen experiment environment (X8, issues #133). plan.lock names every material file with its
// SHA-256 and the environment a main run must match: gateway commit, image IDs of pi and the execution
// sandbox, the checksums of the model catalogue and the skills, and the wording of the rights
// instruction. The runner checks the files offline and the environment through
// GET /api/exp/environment; a main plan starts only when both agree.

package exp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// LockModel is one model of the plan with the provider and the version the catalogue states (X8.3).
type LockModel struct {
	Role     string `json:"role"`
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Version  string `json:"version"`
	API      string `json:"api,omitempty"`
}

// Lock is the content of plan.lock (X8.3). The checksums are lowercase hex SHA-256.
type Lock struct {
	CreatedAt       string `json:"created_at"`
	Plan            string `json:"plan"`
	Purpose         string `json:"purpose"`
	GatewayVersion  string `json:"gateway_version"`
	GatewayCommit   string `json:"gateway_commit"`
	GatewayDescribe string `json:"gateway_describe"`
	ImagePi         string `json:"image_pi"`
	ImageExec       string `json:"image_exec"`
	CatalogSHA256   string `json:"catalog_sha256"` // the effective catalog (file, AGW_LLM_* provider, AGW_MODEL)
	// CatalogFileSHA256 is the checksum of the models.json file alone; informative, the check compares
	// catalog_sha256 only.
	CatalogFileSHA256 string            `json:"catalog_file_sha256,omitempty"`
	SkillsSHA256      string            `json:"skills_sha256"`
	InstructionSHA256 string            `json:"instruction_sha256"`
	APIIndexSHA256    string            `json:"api_index_sha256"`
	InferenceRuntime  string            `json:"inference_runtime,omitempty"` // #237
	ConformanceSHA256 string            `json:"conformance_sha256"`
	Models            []LockModel       `json:"models"`
	Seed              int64             `json:"seed"`
	K                 int               `json:"k"`
	Runs              int               `json:"runs"` // run count: for each cell the tasks it runs, times k (Plan.RunCount)
	Limits            Limits            `json:"limits"`
	Pass1Threshold    float64           `json:"pass1_threshold"`
	Runtimes          map[string]string `json:"runtimes,omitempty"`
	Files             map[string]string `json:"files"`
}

// Environment is the answer of GET /api/exp/environment (X8.5): the identity of the gateway build and
// the images the warm experiment slots actually run.
type Environment struct {
	GatewayVersion  string `json:"gateway_version"`
	GatewayCommit   string `json:"gateway_commit"`
	GatewayDescribe string `json:"gateway_describe"`
	// CatalogSHA256 is the SHA-256 of the effective catalog (models.json, the provider built from AGW_LLM_*,
	// AGW_MODEL and AGW_LLM_EXTRA_HOST, canonical JSON, no key values). CatalogFileSHA256 is the file alone,
	// Catalog the effective catalog itself.
	CatalogSHA256     string            `json:"catalog_sha256"`
	CatalogFileSHA256 string            `json:"catalog_file_sha256,omitempty"`
	Catalog           json.RawMessage   `json:"catalog,omitempty"`
	SkillsSHA256      string            `json:"skills_sha256"`
	InstructionSHA256 string            `json:"instruction_sha256"`
	APIIndexSHA256    string            `json:"api_index_sha256"`
	InferenceRuntime  string            `json:"inference_runtime,omitempty"` // #237: AGW_INFERENCE_RUNTIME
	ImagesPi          []string          `json:"images_pi"`
	ImagesExec        []string          `json:"images_exec"`
	Slots             []EnvironmentSlot `json:"slots"`
}

// EnvironmentSlot is one warm experiment slot with the image IDs of its two containers.
type EnvironmentSlot struct {
	ID        string `json:"id"`
	Variant   string `json:"variant"`
	State     string `json:"state"`
	ChatID    string `json:"chat_id,omitempty"`
	ImagePi   string `json:"image_pi,omitempty"`
	ImageExec string `json:"image_exec,omitempty"`
}

// LoadLock reads plan.lock below dir.
func LoadLock(dir string) (*Lock, error) {
	path := filepath.Join(dir, "plan.lock")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(l.Files) == 0 {
		return nil, fmt.Errorf("%s lists no files", path)
	}
	return &l, nil
}

// CheckFiles verifies the material against the file checksums of the lock (X8.5). It collects every
// deviation instead of stopping at the first, so that a run reports the whole list.
func (l *Lock) CheckFiles(dir string) []Problem {
	var out []Problem
	if l == nil {
		return []Problem{{"plan.lock", "missing"}}
	}
	names := make([]string, 0, len(l.Files))
	for name := range l.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, rel := range names {
		if !lockSafeRelative(rel) {
			out = append(out, Problem{"plan.lock", fmt.Sprintf("%q is not a relative path inside the material", rel)})
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			out = append(out, Problem{"plan.lock", fmt.Sprintf("%s: %v", rel, err)})
			continue
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, l.Files[rel]) {
			out = append(out, Problem{"plan.lock", fmt.Sprintf("%s differs from the frozen material", rel)})
		}
	}
	return out
}

// CompareEnvironment verifies the gateway build and the warm experiment slots against the lock (X8.5).
func CompareEnvironment(l *Lock, env Environment) []Problem {
	var out []Problem
	if l == nil {
		return []Problem{{"plan.lock", "missing"}}
	}
	compare := func(field, want, got string) {
		if want == "" {
			out = append(out, Problem{"plan.lock", fmt.Sprintf("%s is empty; the material was not frozen with an environment", field)})
			return
		}
		if !strings.EqualFold(want, got) {
			out = append(out, Problem{"environment", fmt.Sprintf("%s is %s, the lock froze %s", field, orNone(got), want)})
		}
	}
	compare("gateway_commit", l.GatewayCommit, env.GatewayCommit)
	if l.GatewayDescribe != "" {
		compare("gateway_describe", l.GatewayDescribe, env.GatewayDescribe)
	}
	compare("catalog_sha256", l.CatalogSHA256, env.CatalogSHA256)
	compare("skills_sha256", l.SkillsSHA256, env.SkillsSHA256)
	compare("instruction_sha256", l.InstructionSHA256, env.InstructionSHA256)
	compare("api_index_sha256", l.APIIndexSHA256, env.APIIndexSHA256)
	if l.InferenceRuntime != "" { // locks from before #237 do not have it
		compare("inference_runtime", l.InferenceRuntime, env.InferenceRuntime)
	}
	compareImages := func(field, want string, got []string) {
		if want == "" {
			out = append(out, Problem{"plan.lock", fmt.Sprintf("%s is empty; the material was not frozen with an environment", field)})
			return
		}
		if len(got) == 0 {
			out = append(out, Problem{"environment", fmt.Sprintf("no warm experiment slot reports %s (set AGW_EXPERIMENT_POOL)", field)})
			return
		}
		for _, id := range got {
			if !strings.EqualFold(id, want) {
				out = append(out, Problem{"environment", fmt.Sprintf("%s is %s in a warm slot, the lock froze %s", field, id, want)})
			}
		}
	}
	compareImages("image_pi", l.ImagePi, env.ImagesPi)
	compareImages("image_exec", l.ImageExec, env.ImagesExec)
	return out
}

// CompareSlot verifies the image IDs of the slot that carries one run (X8.5). A slot without image IDs
// cannot be verified and is a deviation.
func CompareSlot(l *Lock, slot EnvironmentSlot) []Problem {
	var out []Problem
	if l == nil {
		return []Problem{{"plan.lock", "missing"}}
	}
	if !strings.EqualFold(l.ImagePi, slot.ImagePi) {
		out = append(out, Problem{"slot " + slot.ID, fmt.Sprintf("image_pi is %s, the lock froze %s", orNone(slot.ImagePi), l.ImagePi)})
	}
	if !strings.EqualFold(l.ImageExec, slot.ImageExec) {
		out = append(out, Problem{"slot " + slot.ID, fmt.Sprintf("image_exec is %s, the lock froze %s", orNone(slot.ImageExec), l.ImageExec)})
	}
	return out
}

// SlotForChat returns the environment slot that carries the chat.
func (e Environment) SlotForChat(chatID string) (EnvironmentSlot, bool) {
	for _, s := range e.Slots {
		if s.ChatID != "" && s.ChatID == chatID {
			return s, true
		}
	}
	return EnvironmentSlot{}, false
}

// FileSHA256 hashes one file.
func FileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// TreeSHA256 hashes the names and contents of all files below root in a stable order. A missing root
// yields an empty string and no error: a local build without the copied skills still starts, and the
// lock check reports the missing checksum.
func TreeSHA256(root string) (string, error) {
	if root == "" {
		return "", nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return treeSHA256(root)
}

func treeSHA256(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(h, "%s\x00%s\x00", rel, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// materialSkipDirs are result folders and local caches: they are results, not material (X8.3). The Python
// freeze step (masterarbeit/versuch/einfrieren.py, SKIP_DIRS) uses the same list.
var materialSkipDirs = map[string]bool{
	".git": true, "replikation": true, "__pycache__": true, ".pytest_cache": true, ".venv": true,
	".uv-cache": true, "ergebnisse": true, "export": true,
}

// materialSkipped reports whether a slash-separated relative path is no material: below a skipped
// directory, the lock itself, or a compiled or compressed artefact.
func materialSkipped(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		if materialSkipDirs[part] {
			return true
		}
	}
	return rel == "plan.lock" || strings.HasSuffix(rel, ".pyc") || strings.HasSuffix(rel, ".zst")
}

// gitTrackedFiles lists the files Git versions below dir, relative to dir and slash-separated. ok is false
// when dir is not inside a Git work tree or git is not available (the server copy comes from git archive
// and carries no .git); the caller then takes every file present.
func gitTrackedFiles(dir string) (files []string, ok bool) {
	inside, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return nil, false
	}
	// Without --full-name, ls-files prints paths relative to dir, and "." limits the list to dir.
	raw, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--", ".").Output()
	if err != nil {
		return nil, false
	}
	for _, name := range strings.Split(string(raw), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, true
}

// MaterialFiles hashes every file of the material that belongs to the experiment (X8.3). Inside a Git work
// tree only the files Git versions count, so an untracked or ignored file (a node_modules folder, a local
// scratch file) never enters the lock: on the server the material arrives as git archive, and a lock over
// files that archive does not carry could never be checked there (BM17 K2). Without Git (the server copy)
// every file present counts. Output folders of runs and local caches stay out in both cases.
func MaterialFiles(dir string) (map[string]string, error) {
	out := map[string]string{}
	if tracked, ok := gitTrackedFiles(dir); ok {
		for _, rel := range tracked {
			if materialSkipped(rel) {
				continue
			}
			path := filepath.Join(dir, filepath.FromSlash(rel))
			if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
				continue // deleted in the work tree, or a submodule
			}
			sum, err := FileSHA256(path)
			if err != nil {
				return nil, err
			}
			out[rel] = sum
		}
		return out, nil
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && materialSkipDirs[filepath.Base(rel)] {
				return fs.SkipDir
			}
			return nil
		}
		if materialSkipped(rel) {
			return nil
		}
		sum, err := FileSHA256(path)
		if err != nil {
			return err
		}
		out[rel] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func lockSafeRelative(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agw/internal/config"
	"agw/internal/exp"
)

const envCatalogFile = `{"default":"ds/flash","providers":[{"id":"ds","upstream":"https://api.example.com","api":"openai-completions",
	"api_key_env":"DS_KEY","models":[{"id":"flash","pi_builtin":true}]}]}`

func envFor(over map[string]string) func(string) string {
	m := map[string]string{"AGW_LLM_PROVIDER": "farm", "AGW_LLM_BASE_URL": "https://llm.example.internal/v1", "AGW_LLM_MODEL_ID": "Org/M",
		"AGW_LLM_API_KEY_ENV": "FARM_KEY", "AGW_LLM_CONTEXT_WINDOW": "262144", "AGW_LLM_MAX_TOKENS": "12288", "FARM_KEY": "very-secret-key-value"}
	for k, v := range over {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

// catalog_sha256 is the checksum of the effective catalogue, so a switch in the .env is visible; the file's own
// checksum and the effective catalogue are reported next to it, and no key value is ever in them.
func TestBuildEnvironmentEffectiveCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(envCatalogFile), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(over map[string]string) exp.Environment {
		t.Helper()
		cat, err := config.LoadEffectiveCatalog(path, envFor(over))
		if err != nil {
			t.Fatal(err)
		}
		return BuildEnvironment(config.Env{CatalogPath: path}, cat)
	}
	a, b := build(nil), build(map[string]string{"AGW_LLM_TAGS": "gb10"})
	fileSHA, _ := exp.FileSHA256(path)
	if a.CatalogFileSHA256 != fileSHA || a.CatalogFileSHA256 != b.CatalogFileSHA256 {
		t.Fatalf("file checksum: %q %q (want %q)", a.CatalogFileSHA256, b.CatalogFileSHA256, fileSHA)
	}
	if a.CatalogSHA256 == "" || a.CatalogSHA256 == a.CatalogFileSHA256 || a.CatalogSHA256 == b.CatalogSHA256 {
		t.Fatalf("effective checksum: %q file %q other %q", a.CatalogSHA256, a.CatalogFileSHA256, b.CatalogSHA256)
	}
	if !json.Valid(a.Catalog) || !strings.Contains(string(a.Catalog), "Org/M") || strings.Contains(string(a.Catalog), "very-secret-key-value") {
		t.Fatalf("effective catalogue: %s", a.Catalog)
	}
	// The lock check compares the effective checksum.
	l := &exp.Lock{CatalogSHA256: a.CatalogSHA256}
	for _, p := range exp.CompareEnvironment(l, b) {
		if strings.Contains(p.Text, "catalog_sha256") {
			return
		}
	}
	t.Fatal("a changed tag did not break the lock check")
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"agw/internal/chat"
	"agw/internal/exp"
	"agw/internal/version"
)

// X8.1: GET /api/config names the build, so a deployment states which gateway it serves.
func TestConfigShowsVersion(t *testing.T) {
	s := &Server{M: chat.NewManager(nil, nil, nil, nil, nil, chat.Options{})}
	w := httptest.NewRecorder()
	s.config(w, httptest.NewRequest("GET", "/api/config", nil))
	var cfg struct {
		Version  string `json:"gateway_version"`
		Commit   string `json:"gateway_commit"`
		Describe string `json:"gateway_describe"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != version.String() || cfg.Commit != version.Commit || cfg.Describe != version.Describe {
		t.Fatalf("version: %+v, want %q/%q/%q", cfg, version.String(), version.Commit, version.Describe)
	}
}

// X8.5: the environment endpoint reports the identity of the build and the checksums the lock freezes.
func TestExpEnvironmentShowsBuildAndChecksums(t *testing.T) {
	s := &Server{Environment: exp.Environment{
		GatewayVersion: "v1", GatewayCommit: "abc123", GatewayDescribe: "abc123",
		CatalogSHA256: "cat", SkillsSHA256: "skills", InstructionSHA256: "instr",
	}}
	w := httptest.NewRecorder()
	s.expEnvironment(w, httptest.NewRequest("GET", "/api/exp/environment", nil))
	var env exp.Environment
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.GatewayVersion != "v1" || env.GatewayCommit != "abc123" || env.CatalogSHA256 != "cat" ||
		env.SkillsSHA256 != "skills" || env.InstructionSHA256 != "instr" {
		t.Fatalf("environment: %+v", env)
	}
	if len(env.ImagesPi) != 0 || len(env.ImagesExec) != 0 || len(env.Slots) != 0 {
		t.Fatalf("without a pool no slot is reported: %+v", env)
	}
}

// Without a filled Server.Environment the endpoint still names the binary and the instruction wording,
// so a local build answers sensibly.
func TestExpEnvironmentFillsIdentity(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).expEnvironment(w, httptest.NewRequest("GET", "/api/exp/environment", nil))
	var env exp.Environment
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.GatewayVersion != version.String() || env.InstructionSHA256 == "" {
		t.Fatalf("environment: %+v", env)
	}
	if env.InstructionSHA256 != chat.RightsInstructionSHA256() {
		t.Fatalf("instruction checksum: %s", env.InstructionSHA256)
	}
}

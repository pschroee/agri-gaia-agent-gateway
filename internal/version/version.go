// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package version carries the identity of the running gateway build (X8.1). The values are set at build
// time with -ldflags "-X agw/internal/version.Commit=<sha> -X agw/internal/version.Describe=<git describe>";
// the Dockerfile takes them from the build arguments AGW_COMMIT and AGW_DESCRIBE.
package version

import (
	"os"
	"runtime/debug"
)

// Commit is the git commit the binary was built from. "dev" for a local build.
var Commit = "dev"

// Describe is `git describe --always --dirty` of the source tree. "dev" for a local build.
var Describe = "dev"

// String names the build in one value: the describe string when there is one, else the commit, else
// AGW_VERSION from the environment (deployments before X8), else the module version of the build, else
// "devel". It is what GET /api/config, the start log, exp_runs.gateway_version and the export manifest
// report.
func String() string {
	if Describe != "" && Describe != "dev" {
		return Describe
	}
	if Commit != "" && Commit != "dev" {
		return Commit
	}
	if v := os.Getenv("AGW_VERSION"); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

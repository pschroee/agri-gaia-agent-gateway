// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package testpg hands out schema names for tests that run against the shared test Postgres.
package testpg

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// Schema returns a schema name that no other test can be using: the prefix, the process id and eight
// random bytes. A name derived from the clock alone is not enough — two test binaries started in the
// same moment picked the same name, created their tables into one schema and dropped it under each
// other, which surfaced as duplicate-key errors and deadlocks while creating the schema (issue #158).
func Schema(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a test cannot do anything sensible with the error.
		panic(err)
	}
	return fmt.Sprintf("%s_%d_%s", prefix, os.Getpid(), hex.EncodeToString(b[:]))
}

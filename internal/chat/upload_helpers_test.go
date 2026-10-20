// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"crypto/sha256"
	"encoding/hex"
)

// upSum is the SHA-256 of a test upload. PlatformCall checks the bytes it sends against the hash of the
// approved manifest (issue #164), so a test upload must carry its real hash.
func upSum(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

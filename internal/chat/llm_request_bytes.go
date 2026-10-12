// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

// requestBytes maps the measured request size of a model call to the stored value (issue #69):
// 0 means not measured and is stored as NULL.
func requestBytes(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

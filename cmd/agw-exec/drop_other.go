// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

//go:build !linux

package main

// Tracing runs only in the Linux execution sandbox; on other systems only the unit tests run.
func dropTraced() error { return nil }

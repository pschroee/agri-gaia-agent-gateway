//go:build !linux

package main

// notDumpable exists only on Linux; on other systems only the unit tests run.
func notDumpable() error { return nil }

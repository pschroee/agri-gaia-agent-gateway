//go:build !linux

package main

// notDumpable gibt es nur unter Linux; auf anderen Systemen laufen nur die Unit-Tests.
func notDumpable() error { return nil }

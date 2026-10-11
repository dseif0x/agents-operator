//go:build !linux

package main

// hideProcess is a no-op off Linux, where the runner only runs for local
// development.
func hideProcess() error { return nil }

//go:build linux

package main

import "syscall"

// hideProcess marks the runner non-dumpable: /proc/<pid>/environ, mem and
// friends become root-owned and ptrace is refused to the agent, which runs
// as the same user without capabilities. See private.go.
func hideProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

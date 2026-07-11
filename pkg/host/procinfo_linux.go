//go:build linux

// procinfo_linux.go — live process introspection, Linux flavor: /proc has
// everything (comm, cwd) with no syscall gymnastics.
package host

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// fgProcess returns the foreground process group leader's (pid, command
// name) for the given PTY master fd. ok=false when the lookup fails.
func fgProcess(ptmxFd int) (pid int, comm string, ok bool) {
	pgid, err := unix.IoctlGetInt(ptmxFd, unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return 0, "", false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgid))
	if err != nil {
		return pgid, "", true // leader raced away; pgid still useful
	}
	return pgid, strings.TrimSpace(string(data)), true
}

// liveCwd reads the process's current working directory from /proc.
func liveCwd(pid int) (string, bool) {
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", false
	}
	return cwd, true
}

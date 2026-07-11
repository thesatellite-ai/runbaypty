//go:build darwin

// procinfo_darwin.go — live process introspection, macOS flavor.
//
// Foreground process: tcgetpgrp on the PTY master gives the foreground
// process GROUP; the group leader's pid == pgid (shells set each job as its
// own group). Its command name comes from sysctl kern.proc.pid (kinfo_proc
// p_comm) — no cgo, no /proc, no external binaries.
//
// Live cwd: macOS only exposes it via proc_pidinfo(PROC_PIDVNODEPATHINFO),
// which x/sys does not wrap and cgo is not worth the cost — INFO falls back
// to the spawn cwd here (Linux reports live). Honest per-platform gap,
// documented on the SessionInfo field.
package host

import (
	"strings"

	"golang.org/x/sys/unix"
)

// fgProcess returns the foreground process group leader's (pid, command
// name) for the given PTY master fd. ok=false when the lookup fails —
// callers fall back to the spawn values, never error a whole INFO over it.
func fgProcess(ptmxFd int) (pid int, comm string, ok bool) {
	pgid, err := unix.IoctlGetInt(ptmxFd, unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return 0, "", false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pgid)
	if err != nil {
		// Group leader may have exited between the two calls; still report
		// the pgid so callers see the group even without a name.
		return pgid, "", true
	}
	name := unix.ByteSliceToString(kp.Proc.P_comm[:])
	return pgid, strings.TrimSpace(name), true
}

// liveCwd is unavailable without cgo on macOS; ok=false always.
func liveCwd(pid int) (string, bool) { return "", false }

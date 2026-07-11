package host

import (
	"testing"
	"time"
)

// TestSession_ForegroundProcessTracking — task-m2-fgproc-t1: the INFO
// snapshot must follow the PTY's foreground job as it changes.
func TestSession_ForegroundProcessTracking(t *testing.T) {
	s := spawnT(t, SpawnConfig{Cmd: "/bin/sh", Cols: 80, Rows: 24})
	s.TakeWrite("cli_t")

	// A shell at rest: the foreground group is the shell itself. /bin/sh's
	// comm differs per platform (macOS: bash; Debian: dash) — accept any.
	shellNames := []string{"sh", "bash", "dash", "zsh"}
	waitFg := func(wants ...string) {
		t.Helper()
		deadline := time.Now().Add(testTimeout)
		for {
			info := s.Info()
			for _, w := range wants {
				if info.FgComm == w {
					if info.FgPid <= 0 {
						t.Fatalf("fg_comm %q with fg_pid %d", info.FgComm, info.FgPid)
					}
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("fg never became %v (last: %q pid %d)", wants, info.FgComm, info.FgPid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFg(shellNames...)

	// Run a foreground job: the fg group must flip to it…
	if err := s.WriteInput("cli_t", []byte("sleep 300\n")); err != nil {
		t.Fatal(err)
	}
	waitFg("sleep")

	// …and back to the shell when the job dies.
	if err := s.WriteInput("cli_t", []byte{0x03}); err != nil { // ^C
		t.Fatal(err)
	}
	waitFg(shellNames...)
}

package daemon

// stress_test.go — the M3 stress gate: many sessions × many clients under
// -race, byte-exact delivery for every subscriber, zero goroutine leaks
// (TestMain's goleak covers the package). Uses the real SDK — this doubles
// as the SDK's concurrency torture test.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// ctxT returns a per-test timeout context.
func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// stressSessions × stressClients — CI-sized but real: 30 PTYs, 120 attach
// pumps, 3600 expected lines audited byte-exact.
const (
	stressSessions = 30
	stressClients  = 4
	stressLines    = 30
)

func TestStress_ManySessionsManyClients(t *testing.T) {
	if testing.Short() {
		t.Skip("stress skipped in -short")
	}
	sock, _ := startServer(t, Options{MaxSessions: stressSessions + 4})

	// Spawner connection creates every session up front.
	boss, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()

	type sessionRec struct {
		id     string
		prefix string // "S007-line-"
		want   string // the final line, proving the block completed
	}
	sessions := make([]sessionRec, stressSessions)
	for i := range sessions {
		// Each session prints its distinct numbered block, then lingers so
		// late attachers still find it running.
		script := fmt.Sprintf(`for j in $(seq 1 %d); do echo S%03d-line-$j; done; sleep 300`, stressLines, i)
		id, _, err := boss.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", script}})
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		sessions[i] = sessionRec{
			id:     id,
			prefix: fmt.Sprintf("S%03d-line-", i),
			want:   fmt.Sprintf("S%03d-line-%d\r\n", i, stressLines),
		}
	}

	// Every client dials its own connection and attaches to EVERY session.
	var wg sync.WaitGroup
	errs := make(chan error, stressSessions*stressClients)
	for cn := range stressClients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl, err := client.Dial(sock)
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", cn, err)
				return
			}
			defer cl.Close()
			var inner sync.WaitGroup
			for _, rec := range sessions {
				inner.Add(1)
				go func() {
					defer inner.Done()
					st, err := cl.Attach(ctxT(t), rec.id, nil, true)
					if err != nil {
						errs <- fmt.Errorf("client %d attach %s: %w", cn, rec.id, err)
						return
					}
					var acc []byte
					buf := make([]byte, 8192)
					deadline := time.Now().Add(testTimeout)
					for !strings.Contains(string(acc), rec.want) {
						if time.Now().After(deadline) {
							errs <- fmt.Errorf("client %d session %s: timeout (%d bytes)", cn, rec.id, len(acc))
							return
						}
						n, rerr := st.Read(buf)
						acc = append(acc, buf[:n]...)
						if rerr != nil {
							errs <- fmt.Errorf("client %d session %s: %w", cn, rec.id, rerr)
							return
						}
					}
					// Byte-exact audit: every line of this session's block
					// present exactly once, in order. (No Sscanf: Go's fmt
					// has no %* suppression verb — match the prefix.)
					norm := strings.ReplaceAll(string(acc), "\r\n", "\n")
					last := 0
					count := 0
					for _, line := range strings.Split(norm, "\n") {
						rest, ok := strings.CutPrefix(line, rec.prefix)
						if !ok {
							continue
						}
						var n int
						if _, err := fmt.Sscanf(rest, "%d", &n); err != nil {
							continue
						}
						if n != last+1 {
							errs <- fmt.Errorf("client %d session %s: line %d after %d (gap/dup/reorder)", cn, rec.id, n, last)
							return
						}
						last = n
						count++
					}
					if count != stressLines {
						errs <- fmt.Errorf("client %d session %s: %d lines, want %d", cn, rec.id, count, stressLines)
					}
				}()
			}
			inner.Wait()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Tear down every session so shutdown is fast and goleak stays clean.
	for _, rec := range sessions {
		_ = boss.Kill(ctxT(t), rec.id, proto.SignalKILL)
	}
}

func TestSocketPermissions(t *testing.T) {
	sock, _ := startServer(t, Options{})
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perms = %o, want 600 — file permissions ARE the UDS auth model", perm)
	}
}

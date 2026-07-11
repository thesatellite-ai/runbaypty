package daemon

// soak_test.go — task-x-soak: a long-running daemon with steady session
// churn and output must hold FLAT goroutine and heap curves. Skipped unless
// RUNBAYPTY_SOAK is set to a duration ("10m" in nightly CI, "20s" for a
// local sanity pass) — the default test run never pays for it.

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestSoak_FlatGoroutinesAndHeap(t *testing.T) {
	durStr := os.Getenv("RUNBAYPTY_SOAK")
	if durStr == "" {
		t.Skip("set RUNBAYPTY_SOAK=10m (or 20s) to run the soak")
	}
	dur, err := time.ParseDuration(durStr)
	if err != nil {
		t.Fatalf("RUNBAYPTY_SOAK %q: %v", durStr, err)
	}

	// Short retention: churned corpses must flow THROUGH the reaper (500ms
	// tick), exercising retention under churn — with the 10-minute default
	// they would pool up to max-sessions in under a minute.
	//
	// MaxSessions is set generously (not as an assertion — this test measures
	// goroutine/heap flatness, not the cap). At ~20 spawns/sec into a 2s
	// retention window the live pool sits near 20 long-lived + ~40 lingering;
	// on a loaded CI runner the 500ms reaper can transiently fall a few behind,
	// so a tight cap (e.g. 64) spuriously trips E_LIMIT_EXCEEDED. A real leak
	// still fails via the goroutine/heap assertions below, which measure at
	// quiescence, so the high cap hides nothing.
	sock, _ := startServer(t, Options{MaxSessions: 512, RetentionTTL: 2 * time.Second})
	boss, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()

	// 20 chattering long-lived sessions, each printing every 100ms.
	longLived := make([]string, 20)
	for i := range longLived {
		id, _, err := boss.Spawn(ctxT(t), client.SpawnOpts{
			Cmd: "/bin/sh", Args: []string{"-c", `while :; do echo soak-tick; sleep 0.1; done`},
		})
		if err != nil {
			t.Fatal(err)
		}
		longLived[i] = id
	}
	// One reader following a session (keeps a pump busy the whole run).
	st, err := boss.Attach(ctxT(t), longLived[0], nil, true)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 8192)
		for {
			if _, err := st.Read(buf); err != nil {
				return
			}
		}
	}()

	// Baseline AFTER warmup so one-time allocations don't count.
	time.Sleep(2 * time.Second)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	g0 := runtime.NumGoroutine()

	// Churn loop: spawn/exit short sessions continuously until the deadline.
	deadline := time.Now().Add(dur)
	churned := 0
	for time.Now().Before(deadline) {
		id, _, err := boss.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo churn; exit 0"}})
		if err != nil {
			t.Fatalf("churn spawn %d: %v", churned, err)
		}
		churned++
		time.Sleep(50 * time.Millisecond)
		_ = boss.Kill(ctxT(t), id, proto.SignalKILL) // idempotent if already gone
	}

	// Let retention/pumps settle, then measure.
	time.Sleep(2 * time.Second)
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	g1 := runtime.NumGoroutine()

	t.Logf("soak %v: churned %d sessions · goroutines %d→%d · heap %dKB→%dKB",
		dur, churned, g0, g1, m0.HeapAlloc/1024, m1.HeapAlloc/1024)

	// Goroutines: allow small jitter (retention reaper timing), never growth
	// proportional to churn.
	if g1 > g0+10 {
		t.Errorf("goroutines grew %d → %d over %d churned sessions — leak", g0, g1, churned)
	}
	// Heap: exited sessions retain their rings until the TTL reaps them, so
	// allow the retention window's worth; flag anything beyond 64 MiB drift.
	if growth := int64(m1.HeapAlloc) - int64(m0.HeapAlloc); growth > 64<<20 {
		t.Errorf("heap grew %d MiB over the soak — leak", growth>>20)
	}

	for _, id := range longLived {
		_ = boss.Kill(ctxT(t), id, proto.SignalKILL)
	}
}

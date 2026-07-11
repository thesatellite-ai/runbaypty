package daemon

// bench_test.go — the firehose benchmark (task-m3-bench): full-pipeline
// throughput PTY → reader → ring → pump → UDS → SDK stream. Run via
// `task bench`; numbers land in BENCH.md.

import (
	"context"
	"io"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// benchPayloadBytes per iteration: 64 MiB of /dev/zero through the whole
// stack. dd exits when done, so the stream EOFs cleanly.
const benchPayloadBytes = 64 << 20

func BenchmarkFirehose64MiB(b *testing.B) {
	sockDir, err := mkdirTempShort()
	if err != nil {
		b.Fatal(err)
	}
	defer removeAllQuiet(sockDir)
	srv, err := New(Options{HomeDir: b.TempDir(), SocketPath: sockDir + "/d.sock", Version: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-srv.Ready()
	defer func() { cancel(); <-done }()

	cl, err := client.Dial(sockDir + "/d.sock")
	if err != nil {
		b.Fatal(err)
	}
	defer cl.Close()

	b.SetBytes(benchPayloadBytes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, _, err := cl.Spawn(context.Background(), client.SpawnOpts{
			Cmd:  "/bin/sh",
			Args: []string{"-c", "dd if=/dev/zero bs=65536 count=1024 2>/dev/null"},
			// A big ring so the pump (not ring truncation) is what's measured.
			RingBytes: 8 << 20,
		})
		if err != nil {
			b.Fatal(err)
		}
		st, err := cl.Attach(context.Background(), id, nil, true)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, st)
		if err != nil {
			b.Fatal(err)
		}
		if n < benchPayloadBytes {
			b.Fatalf("streamed %d bytes, want ≥ %d", n, benchPayloadBytes)
		}
		b.StopTimer()
		_ = cl.Kill(context.Background(), id, proto.SignalKILL)
		b.StartTimer()
	}
}

// BenchmarkEchoLatency measures the single-keystroke round trip:
// INPUT → PTY echo → OUTPUT back at the client.
func BenchmarkEchoLatency(b *testing.B) {
	sockDir, err := mkdirTempShort()
	if err != nil {
		b.Fatal(err)
	}
	defer removeAllQuiet(sockDir)
	srv, err := New(Options{HomeDir: b.TempDir(), SocketPath: sockDir + "/d.sock", Version: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-srv.Ready()
	defer func() { cancel(); <-done }()

	cl, err := client.Dial(sockDir + "/d.sock")
	if err != nil {
		b.Fatal(err)
	}
	defer cl.Close()
	id, _, err := cl.Spawn(context.Background(), client.SpawnOpts{Cmd: "/bin/cat"})
	if err != nil {
		b.Fatal(err)
	}
	st, err := cl.Attach(context.Background(), id, nil, false)
	if err != nil {
		b.Fatal(err)
	}
	if err := cl.TakeWrite(context.Background(), id); err != nil {
		b.Fatal(err)
	}

	buf := make([]byte, 256)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cl.Input(id, []byte("x")); err != nil {
			b.Fatal(err)
		}
		if _, err := st.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_ = cl.Kill(context.Background(), id, proto.SignalKILL)
}

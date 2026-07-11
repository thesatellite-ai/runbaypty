package host

import (
	"bytes"
	"sync"
	"testing"
)

// refByte is the deterministic reference stream: the byte at absolute seq s.
// A prime modulus avoids alignment artifacts with power-of-two ring sizes,
// so any correctly-replayed window is byte-exact verifiable from seq alone.
func refByte(s uint64) byte { return byte(s % 251) }

// refChunk builds the reference bytes for seq range [from, from+n).
func refChunk(from uint64, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = refByte(from + uint64(i))
	}
	return out
}

// verifyReplay asserts ReplayFrom(since) returns exactly the reference
// stream from max(since, oldest) to end.
func verifyReplay(t *testing.T, r *Ring, since uint64, wantFrom uint64, wantTruncated bool) {
	t.Helper()
	data, fromSeq, truncated := r.ReplayFrom(since)
	if fromSeq != wantFrom {
		t.Fatalf("ReplayFrom(%d) fromSeq = %d, want %d", since, fromSeq, wantFrom)
	}
	if truncated != wantTruncated {
		t.Fatalf("ReplayFrom(%d) truncated = %v, want %v", since, truncated, wantTruncated)
	}
	if want := refChunk(fromSeq, int(r.LastSeq()-fromSeq)); !bytes.Equal(data, want) {
		t.Fatalf("ReplayFrom(%d) returned wrong bytes (len %d, want %d)", since, len(data), len(want))
	}
}

// fill writes the reference stream into r in chunks of the given sizes,
// starting at r's current LastSeq, and returns the new LastSeq.
func fill(r *Ring, sizes ...int) uint64 {
	for _, n := range sizes {
		r.Write(refChunk(r.LastSeq(), n))
	}
	return r.LastSeq()
}

func TestRing_ReplayExactMidRing(t *testing.T) {
	t.Parallel()
	r := NewRing(1024)
	end := fill(r, 100, 200, 50) // 350 bytes, no wrap
	if end != 350 {
		t.Fatalf("LastSeq = %d, want 350", end)
	}
	verifyReplay(t, r, 0, 0, false)     // full replay
	verifyReplay(t, r, 123, 123, false) // mid-stream, odd offset
	verifyReplay(t, r, 349, 349, false) // last byte only
}

func TestRing_ReplayAtEndReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := NewRing(64)
	end := fill(r, 40)
	data, fromSeq, truncated := r.ReplayFrom(end)
	if len(data) != 0 || fromSeq != end || truncated {
		t.Fatalf("ReplayFrom(end) = (%d bytes, %d, %v), want (0, %d, false)", len(data), fromSeq, truncated, end)
	}
	// Beyond end (client claims a future seq — bogus but must not panic).
	if data, _, _ := r.ReplayFrom(end + 100); len(data) != 0 {
		t.Fatalf("ReplayFrom(beyond end) returned %d bytes, want 0", len(data))
	}
}

func TestRing_WrapAroundKeepsNewest(t *testing.T) {
	t.Parallel()
	r := NewRing(256)
	end := fill(r, 100, 100, 100, 100) // 400 bytes through a 256 ring
	if end != 400 {
		t.Fatalf("LastSeq = %d, want 400", end)
	}
	if r.Len() != 256 {
		t.Fatalf("Len = %d, want 256", r.Len())
	}
	oldest := end - 256
	verifyReplay(t, r, oldest, oldest, false) // oldest retained byte, exact
	verifyReplay(t, r, oldest+7, oldest+7, false)
	verifyReplay(t, r, 0, oldest, true)        // predates ring → truncated
	verifyReplay(t, r, oldest-1, oldest, true) // one byte too old → truncated
}

func TestRing_SingleWriteLargerThanCapacity(t *testing.T) {
	t.Parallel()
	r := NewRing(128)
	r.Write(refChunk(0, 1000)) // one write, 8× the ring
	if r.LastSeq() != 1000 {
		t.Fatalf("LastSeq = %d, want 1000 (seq tracks the stream, not retention)", r.LastSeq())
	}
	if r.Len() != 128 {
		t.Fatalf("Len = %d, want 128", r.Len())
	}
	verifyReplay(t, r, 0, 1000-128, true)
	// Subsequent normal writes continue seamlessly.
	fill(r, 60)
	verifyReplay(t, r, 1000, 1000, false)
}

func TestRing_ChunkSizeSweep(t *testing.T) {
	t.Parallel()
	// Sweep write sizes around the capacity boundary — off-by-ones in the
	// circular copy show up here or nowhere.
	for _, capBytes := range []int{1, 2, 3, 64, 127, 128} {
		r := NewRing(capBytes)
		for _, n := range []int{1, capBytes - 1, capBytes, capBytes + 1, 3, 2*capBytes + 3} {
			if n <= 0 {
				continue
			}
			fill(r, n)
			oldest := r.LastSeq() - uint64(r.Len())
			verifyReplay(t, r, oldest, oldest, false)
			if oldest > 0 {
				verifyReplay(t, r, 0, oldest, true)
			}
		}
	}
}

func TestRing_SeqMonotonic10M(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("10M-write sweep skipped in -short")
	}
	r := NewRing(4096)
	var prev uint64
	chunk := refChunk(0, 7) // content irrelevant here; seq math is the test
	for range 10_000_000 / 7 {
		r.Write(chunk)
		if got := r.LastSeq(); got != prev+7 {
			t.Fatalf("seq jumped: %d → %d", prev, got)
		} else {
			prev = got
		}
	}
}

func TestRing_ConcurrentWriterAndReplayers(t *testing.T) {
	t.Parallel()
	r := NewRing(512)
	done := make(chan struct{})
	var wg sync.WaitGroup

	// One writer pushing the reference stream…
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for range 5000 {
			fill(r, 13)
		}
	}()
	// …four replayers hammering reads. Every replay must be internally
	// consistent with the reference stream regardless of interleaving.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				data, fromSeq, _ := r.ReplayFrom(0)
				for i, b := range data {
					if b != refByte(fromSeq+uint64(i)) {
						t.Errorf("replay byte at seq %d corrupted", fromSeq+uint64(i))
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewRing_ZeroCapPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("NewRing(0) should panic — programmer error, callers substitute the default first")
		}
	}()
	NewRing(0)
}

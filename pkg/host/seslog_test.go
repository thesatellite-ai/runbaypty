package host

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

func TestSessionLog_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.log")
	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	w, err := NewSessionLogWriter(path, start)
	if err != nil {
		t.Fatal(err)
	}
	recs := []struct {
		at   time.Time
		data []byte
	}{
		{start, []byte("first")},
		{start.Add(50 * time.Millisecond), []byte("second\r\n")},
		{start.Add(3 * time.Second), bytes.Repeat([]byte{0x00, 0xff, 0x1b}, 100)}, // binary-safe
		{start.Add(3 * time.Second), []byte("same-ms")},                           // zero delta
	}
	for _, rec := range recs {
		if err := w.Append(rec.at, rec.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSessionLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(recs) {
		t.Fatalf("read %d records, want %d", len(got), len(recs))
	}
	for i, rec := range recs {
		if !got[i].At.Equal(rec.at) {
			t.Errorf("record %d At = %v, want %v", i, got[i].At, rec.at)
		}
		if !bytes.Equal(got[i].Data, rec.data) {
			t.Errorf("record %d data mismatch (%d vs %d bytes)", i, len(got[i].Data), len(rec.data))
		}
	}
	// Concatenated payloads must reproduce the exact byte stream.
	var stream, want bytes.Buffer
	for i := range got {
		stream.Write(got[i].Data)
		want.Write(recs[i].data)
	}
	if !bytes.Equal(stream.Bytes(), want.Bytes()) {
		t.Error("concatenated replay is not byte-exact")
	}
}

func TestSessionLog_TornFinalRecordDropped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "torn.log")
	start := time.Now().UTC().Truncate(time.Millisecond)

	w, err := NewSessionLogWriter(path, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(start, []byte("complete-record")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(start.Add(time.Second), bytes.Repeat([]byte("x"), 500)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate kill -9 mid-write: truncate inside the second record.
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, full[:len(full)-200], 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSessionLog(path)
	if err != nil {
		t.Fatalf("torn log must read cleanly, got %v", err)
	}
	if len(got) != 1 || string(got[0].Data) != "complete-record" {
		t.Fatalf("expected exactly the complete record, got %d records", len(got))
	}
}

func TestSessionLog_RejectsGarbageAndWrongVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	garbage := filepath.Join(dir, "garbage.log")
	if err := os.WriteFile(garbage, []byte("not a runbaypty log"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionLog(garbage); !errcodes.IsCode(err, errcodes.BadFrame) {
		t.Errorf("garbage: expected E_BAD_FRAME, got %v", err)
	}

	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionLog(empty); !errcodes.IsCode(err, errcodes.BadFrame) {
		t.Errorf("empty: expected E_BAD_FRAME, got %v", err)
	}

	// Future format version refused with the honest code.
	future := filepath.Join(dir, "future.log")
	if err := os.WriteFile(future, append([]byte("RPTY"), 0x63), 0o600); err != nil { // version 99
		t.Fatal(err)
	}
	if _, err := ReadSessionLog(future); !errcodes.IsCode(err, errcodes.Unsupported) {
		t.Errorf("future version: expected E_UNSUPPORTED, got %v", err)
	}
}

func TestSession_DurableLogCapturesOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	cfg := shCfg("echo logged-line-one; echo logged-line-two")
	cfg.LogPath = path
	s := spawnT(t, cfg)
	waitExit(t, s)

	// finish() closed the log; the file is complete now.
	recs, err := ReadSessionLog(path)
	if err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	for _, rec := range recs {
		all.Write(rec.Data)
	}
	for _, want := range []string{"logged-line-one", "logged-line-two"} {
		if !bytes.Contains(all.Bytes(), []byte(want)) {
			t.Errorf("log missing %q; got %q", want, all.String())
		}
	}
	// Log bytes must equal the ring bytes exactly (same stream, two sinks).
	ringData, _, _ := s.ReplayFrom(0)
	if !bytes.Equal(all.Bytes(), ringData) {
		t.Errorf("log (%d bytes) diverges from ring (%d bytes)", all.Len(), len(ringData))
	}
}

func TestSpawn_UnwritableLogPathFailsLoudly(t *testing.T) {
	r := NewRegistry(0)
	cfg := shCfg("true")
	cfg.LogPath = "/nonexistent-dir-xyz/session.log"
	if _, err := r.Spawn(cfg); !errcodes.IsCode(err, errcodes.SpawnFailed) {
		t.Errorf("expected E_SPAWN_FAILED for unwritable log, got %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("failed spawn left registry entries")
	}
}

func TestReopenSessionLogWriter_AppendsWithoutNewHeader(t *testing.T) {
	// Daemon handover reopens an existing log: no second header, records
	// keep accumulating, and the reader sees one continuous stream.
	path := filepath.Join(t.TempDir(), "reopen.log")
	start := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)

	w, err := NewSessionLogWriter(path, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(start, []byte("before-handover")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := ReopenSessionLogWriter(path)
	if err != nil {
		t.Fatalf("ReopenSessionLogWriter: %v", err)
	}
	if err := w2.Append(time.Now(), []byte("after-handover")); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	recs, err := ReadSessionLog(path)
	if err != nil {
		t.Fatalf("reopened log unreadable: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("read %d records, want 2 (a second header would break parsing)", len(recs))
	}
	if string(recs[0].Data) != "before-handover" || string(recs[1].Data) != "after-handover" {
		t.Errorf("records = %q, %q", recs[0].Data, recs[1].Data)
	}
	// Reopening a missing log errors rather than creating one.
	if _, err := ReopenSessionLogWriter(filepath.Join(t.TempDir(), "nope.log")); err == nil {
		t.Error("reopen of a missing log should error")
	}
}

func TestSessionLogWriter_AppendAfterCloseRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.log")
	w, err := NewSessionLogWriter(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(time.Now(), []byte("x")); err == nil {
		t.Error("Append after Close should error")
	}
	if err := w.Close(); err != nil { // idempotent
		t.Errorf("double Close: %v", err)
	}
}

func TestSessionLogWriter_CloseSurfacesFlushFailure(t *testing.T) {
	// A log whose underlying file is closed under it must FAIL on Close —
	// silently losing the tail defeats the purpose of a durable log.
	path := filepath.Join(t.TempDir(), "flushfail.log")
	w, err := NewSessionLogWriter(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Buffer a record (64 KiB buffer means nothing has hit the fd yet)…
	if err := w.Append(time.Now(), []byte("buffered")); err != nil {
		t.Fatal(err)
	}
	// …then yank the file out from under the buffer.
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Error("Close() swallowed a flush failure")
	}
}

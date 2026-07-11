// seslog.go — the opt-in durable per-session output log.
//
// Record format (repeating):
//
//	uvarint tsDeltaMs   — milliseconds since the previous record (first
//	                      record: since the log's start timestamp)
//	uvarint payloadLen
//	payload bytes
//
// preceded once by a fixed header:
//
//	magic "RPTY" · uvarint formatVersion(1) · uvarint startUnixMs
//
// Timestamps from day one make asciinema export (task-m8-asciinema) a pure
// file transform. Deltas keep records ~3 bytes of overhead at steady state.
//
// Crash honesty: a daemon killed mid-write leaves a torn final record. The
// reader detects it (unexpected EOF inside a record) and stops cleanly at
// the last complete record — history is everything up to the crash, never
// garbage.
package host

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// seslogMagic identifies a runbaypty session log.
var seslogMagic = []byte("RPTY")

// seslogFormatVersion is bumped only on incompatible record changes.
const seslogFormatVersion = 1

// SessionLogWriter appends timestamped output records to one file. The
// daemon is the only writer (MISSION: one writer owns the file; clients
// read-only). Not safe for concurrent use — the session's read pump is the
// single caller.
type SessionLogWriter struct {
	f      *os.File
	w      *bufio.Writer
	lastMs int64
	closed bool
}

// NewSessionLogWriter creates (or truncates) the log at path and writes the
// header. start stamps the log's time origin.
func NewSessionLogWriter(path string, start time.Time) (*SessionLogWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, errcodes.Newf(errcodes.SpawnFailed, "open session log %s: %v", path, err).WithCause(err)
	}
	w := bufio.NewWriterSize(f, 64*1024)
	startMs := start.UTC().UnixMilli()
	var head []byte
	head = append(head, seslogMagic...)
	head = binary.AppendUvarint(head, seslogFormatVersion)
	head = binary.AppendUvarint(head, uint64(startMs))
	if _, err := w.Write(head); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seslog: write header: %w", err)
	}
	return &SessionLogWriter{f: f, w: w, lastMs: startMs}, nil
}

// ReopenSessionLogWriter opens an EXISTING log for appending (daemon
// handover): no new header; the delta clock re-bases on the current time,
// which the uvarint delta format tolerates (clamped ≥ 0 per Append).
func ReopenSessionLogWriter(path string) (*SessionLogWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("seslog: reopen %s: %w", path, err)
	}
	return &SessionLogWriter{
		f:      f,
		w:      bufio.NewWriterSize(f, 64*1024),
		lastMs: time.Now().UTC().UnixMilli(),
	}, nil
}

// Append writes one output record stamped at ts.
func (l *SessionLogWriter) Append(ts time.Time, p []byte) error {
	if l.closed {
		return errors.New("seslog: append after close")
	}
	ms := ts.UTC().UnixMilli()
	delta := ms - l.lastMs
	if delta < 0 {
		delta = 0 // clock went backwards; clamp — order beats precision here
	} else {
		l.lastMs = ms
	}
	var rec []byte
	rec = binary.AppendUvarint(rec, uint64(delta))
	rec = binary.AppendUvarint(rec, uint64(len(p)))
	if _, err := l.w.Write(rec); err != nil {
		return fmt.Errorf("seslog: write record head: %w", err)
	}
	if _, err := l.w.Write(p); err != nil {
		return fmt.Errorf("seslog: write record payload: %w", err)
	}
	return nil
}

// Close flushes and closes the file. The flush error is surfaced — for a
// log file, losing the tail silently defeats the point.
func (l *SessionLogWriter) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	if err := l.w.Flush(); err != nil {
		_ = l.f.Close()
		return fmt.Errorf("seslog: flush on close: %w", err)
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("seslog: close: %w", err)
	}
	return nil
}

// SessionLogRecord is one replayed record.
type SessionLogRecord struct {
	// At is the absolute record timestamp (reconstructed from deltas).
	At time.Time
	// Data is the output payload.
	Data []byte
}

// ReadSessionLog replays every complete record from path. A torn final
// record (daemon crash mid-write) is detected and silently dropped — the
// returned records are everything that was durably written.
func ReadSessionLog(path string) ([]SessionLogRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("seslog: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read path: close error carries no information

	r := bufio.NewReaderSize(f, 64*1024)
	head := make([]byte, len(seslogMagic))
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, errcodes.Newf(errcodes.BadFrame, "seslog %s: missing header: %v", path, err)
	}
	if string(head) != string(seslogMagic) {
		return nil, errcodes.Newf(errcodes.BadFrame, "seslog %s: bad magic %q", path, head)
	}
	ver, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, errcodes.Newf(errcodes.BadFrame, "seslog %s: version: %v", path, err)
	}
	if ver != seslogFormatVersion {
		return nil, errcodes.Newf(errcodes.Unsupported, "seslog %s: format v%d, this binary reads v%d", path, ver, seslogFormatVersion)
	}
	startMs, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, errcodes.Newf(errcodes.BadFrame, "seslog %s: start ts: %v", path, err)
	}

	var out []SessionLogRecord
	at := int64(startMs)
	for {
		delta, err := binary.ReadUvarint(r)
		if err != nil {
			break // clean EOF at a record boundary — or torn head: done either way
		}
		plen, err := binary.ReadUvarint(r)
		if err != nil {
			break // torn between head fields
		}
		data := make([]byte, plen)
		if _, err := io.ReadFull(r, data); err != nil {
			break // torn payload — drop the partial record
		}
		at += int64(delta)
		out = append(out, SessionLogRecord{At: time.UnixMilli(at).UTC(), Data: data})
	}
	return out, nil
}

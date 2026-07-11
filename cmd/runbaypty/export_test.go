package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/host"
)

func TestExport_AsciinemaCastV2(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "s.log")
	start := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)

	w, err := host.NewSessionLogWriter(logPath, start)
	if err != nil {
		t.Fatal(err)
	}
	must := func(e error) {
		if e != nil {
			t.Fatal(e)
		}
	}
	must(w.Append(start, []byte("$ make\r\n")))
	must(w.Append(start.Add(1500*time.Millisecond), []byte("building\r\n")))
	must(w.Append(start.Add(3*time.Second), []byte("done \x1b[32mok\x1b[0m\r\n")))
	must(w.Close())

	castPath := filepath.Join(dir, "out.cast")
	if _, err := runCLI(t, "/tmp/unused.sock", "", "export", logPath, "--out", castPath, "--width", "120", "--height", "40", "--title", "build run"); err != nil {
		t.Fatalf("export: %v", err)
	}

	raw, err := os.ReadFile(castPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 { // header + 3 records
		t.Fatalf("cast has %d lines, want 4:\n%s", len(lines), raw)
	}

	var head map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &head); err != nil {
		t.Fatalf("header: %v", err)
	}
	if head["version"].(float64) != 2 || head["width"].(float64) != 120 || head["title"] != "build run" {
		t.Errorf("header = %v", head)
	}
	if head["timestamp"].(float64) != float64(start.Unix()) {
		t.Errorf("timestamp = %v, want %d", head["timestamp"], start.Unix())
	}

	// Each record: [elapsedSeconds, "o", data]; elapsed strictly ordered.
	prev := -1.0
	for i, line := range lines[1:] {
		var rec []any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		elapsed := rec[0].(float64)
		if rec[1] != "o" || elapsed < prev {
			t.Errorf("record %d malformed: %v", i, rec)
		}
		prev = elapsed
	}
	// Timing preserved: second record at 1.5s, third at 3s.
	var rec2, rec3 []any
	_ = json.Unmarshal([]byte(lines[2]), &rec2)
	_ = json.Unmarshal([]byte(lines[3]), &rec3)
	if rec2[0].(float64) != 1.5 || rec3[0].(float64) != 3.0 {
		t.Errorf("timing lost: %v, %v", rec2[0], rec3[0])
	}
	// ANSI passes through into the data field (JSON-escaped ESC).
	if !strings.Contains(lines[3], "\\u001b[32m") {
		t.Errorf("ANSI not preserved: %s", lines[3])
	}
}

func TestExport_EmptyAndMissingLogs(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.log")
	w, err := host.NewSessionLogWriter(empty, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "/tmp/unused.sock", "", "export", empty); err == nil {
		t.Error("empty log should refuse with a hint")
	}
	if _, err := runCLI(t, "/tmp/unused.sock", "", "export", filepath.Join(dir, "nope.log")); err == nil {
		t.Error("missing log should error")
	}
}

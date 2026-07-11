package main

// verbs_e2e_test.go — the CLI verbs the main e2e suite doesn't drive:
// events (streaming), lastcmd (OSC 133), serve's flag surface, dial's
// socket resolution, and the JSON output shapes.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestCLI_EventsStreamsUntilContextEnds(t *testing.T) {
	sock := startDaemonT(t)

	// `events` blocks forever; run it with a short context and assert it
	// printed the lifecycle of a session created while it watched.
	done := make(chan string, 1)
	go func() {
		root := newRootCommand()
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		root.SetArgs([]string{"--sock", sock, "events"})
		_ = root.ExecuteContext(ctx)
		done <- out.String()
	}()

	time.Sleep(200 * time.Millisecond) // let the watcher subscribe
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/sh", "-c", "exit 7"))

	var out string
	select {
	case out = <-done:
	case <-time.After(testTimeout):
		t.Fatal("events never returned")
	}
	for _, want := range []string{string(proto.EventCreated), string(proto.EventExited), id[:12]} {
		if !strings.Contains(out, want) {
			t.Errorf("events output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "exit_code=7") {
		t.Errorf("events did not render event data:\n%s", out)
	}
}

func TestCLI_EventsJSONAndSessionFilter(t *testing.T) {
	sock := startDaemonT(t)
	done := make(chan string, 1)
	go func() {
		root := newRootCommand()
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		root.SetArgs([]string{"--sock", sock, "events", "--json"})
		_ = root.ExecuteContext(ctx)
		done <- out.String()
	}()
	time.Sleep(200 * time.Millisecond)
	mustCLI(t, sock, "run", "--", "/bin/sh", "-c", "exit 0")

	var out string
	select {
	case out = <-done:
	case <-time.After(testTimeout):
		t.Fatal("events --json never returned")
	}
	// One JSON object per line, each parseable with the documented fields.
	saw := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var ev struct {
			Type      string            `json:"Type"`
			SessionID string            `json:"SessionID"`
			Data      map[string]string `json:"Data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("events --json emitted non-JSON line %q: %v", line, err)
		}
		if ev.Type == "" {
			t.Errorf("event without a type: %s", line)
		}
		saw++
	}
	if saw == 0 {
		t.Fatal("events --json printed nothing")
	}
}

func TestCLI_LastCmdVerb(t *testing.T) {
	sock := startDaemonT(t)
	// A session emitting OSC 133 marks around one command's output.
	script := `printf '\033]133;C\007LASTCMD-BODY\033]133;D;0\007'; sleep 300`
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/sh", "-c", script))

	deadline := time.Now().Add(testTimeout)
	var out string
	for {
		got, err := runCLI(t, sock, "", "lastcmd", id)
		if err == nil && strings.Contains(got, "LASTCMD-BODY") {
			out = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lastcmd never returned the command body (last: %q, err %v)", got, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !strings.Contains(out, "LASTCMD-BODY") {
		t.Errorf("lastcmd output = %q", out)
	}
	// A session without marks errors with the shell-integration hint.
	plain := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/sh", "-c", "sleep 300"))
	if _, err := runCLI(t, sock, "", "lastcmd", plain); !errcodes.IsCode(err, errcodes.NotFound) {
		t.Errorf("lastcmd on unmarked session = %v, want E_NOT_FOUND", err)
	}
	mustCLI(t, sock, "kill", "--signal", "KILL", id)
	mustCLI(t, sock, "kill", "--signal", "KILL", plain)
}

func TestCLI_DialResolvesSocketFromEnv(t *testing.T) {
	sock := startDaemonT(t)
	// With no --sock flag, dial falls back to $RUNBAYPTY_SOCK.
	t.Setenv(constants.EnvSock, sock)
	root := newRootCommand()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"ls"})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("ls via $%s: %v\n%s", constants.EnvSock, err, out.String())
	}
	if !strings.Contains(out.String(), "no sessions") {
		t.Errorf("unexpected ls output: %s", out.String())
	}
}

func TestCLI_RunJSONAndFlagPlumbing(t *testing.T) {
	sock := startDaemonT(t)
	raw := mustCLI(t, sock, "run", "--json", "--name", "flagged", "--cwd", "/tmp",
		"--env", "RPTY_TEST=1", "--meta", "k=v", "--cols", "100", "--rows", "30",
		"--ring", "65536", "--no-linger", "--", "/bin/sh", "-c", "sleep 300")
	var spawned struct {
		ID   string `json:"id"`
		Pid  int    `json:"pid"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &spawned); err != nil {
		t.Fatalf("run --json: %v (%q)", err, raw)
	}
	if !strings.HasPrefix(spawned.ID, "ses_") || spawned.Pid <= 0 || spawned.Name != "flagged" {
		t.Fatalf("run --json = %+v", spawned)
	}

	var info proto.SessionInfo
	if err := json.Unmarshal([]byte(mustCLI(t, sock, "info", "flagged", "--json")), &info); err != nil {
		t.Fatal(err)
	}
	if info.Cols != 100 || info.Rows != 30 || info.Meta["k"] != "v" {
		t.Errorf("flags not plumbed: %dx%d meta=%v", info.Cols, info.Rows, info.Meta)
	}
	// Malformed --meta is rejected before dialing.
	if _, err := runCLI(t, sock, "", "run", "--meta", "no-equals", "--", "/bin/sh"); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("bad --meta = %v, want E_INVALID_INPUT", err)
	}
	// Malformed resize args are rejected before dialing.
	if _, err := runCLI(t, sock, "", "resize", "flagged", "abc", "24"); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("bad resize cols = %v", err)
	}
	if _, err := runCLI(t, sock, "", "resize", "flagged", "80", "xyz"); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("bad resize rows = %v", err)
	}
	mustCLI(t, sock, "kill", "--signal", "KILL", "flagged")
}

func TestCLI_MetaGetAndInfoRendering(t *testing.T) {
	sock := startDaemonT(t)
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--name", "rendered", "--meta", "b=2", "--meta", "a=1", "--", "/bin/sh", "-c", "sleep 300"))

	// Human info renders sorted meta keys + the core fields.
	info := mustCLI(t, sock, "info", "rendered")
	for _, want := range []string{"id:", id, "state:", "running", "size:", "started:", "bytes:", "clients:", "meta.a: 1", "meta.b: 2"} {
		if !strings.Contains(info, want) {
			t.Errorf("info missing %q:\n%s", want, info)
		}
	}
	if strings.Index(info, "meta.a") > strings.Index(info, "meta.b") {
		t.Error("meta keys not sorted in info output")
	}
	// meta get returns exactly the map.
	var meta map[string]string
	if err := json.Unmarshal([]byte(mustCLI(t, sock, "meta", "get", "rendered")), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta) != 2 || meta["a"] != "1" {
		t.Errorf("meta get = %v", meta)
	}
	// Bad k=v on `meta set` is rejected.
	if _, err := runCLI(t, sock, "", "meta", "set", "rendered", "nope"); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("meta set bad pair = %v", err)
	}
	mustCLI(t, sock, "kill", "--signal", "KILL", id)
}

func TestCLI_TailNoFollowAndErrors(t *testing.T) {
	sock := startDaemonT(t)
	// tail --no-follow over a ring-only session prints history then exits.
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/sh", "-c", "echo tail-nofollow; exit 0"))
	deadline := time.Now().Add(testTimeout)
	for !strings.Contains(mustCLI(t, sock, "info", id, "--json"), `"exited"`) {
		if time.Now().After(deadline) {
			t.Fatal("session never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := runCLI(t, sock, "", "tail", "--no-follow", id)
	if err != nil {
		t.Fatalf("tail --no-follow: %v\n%s", err, out)
	}
	if !strings.Contains(out, "tail-nofollow") {
		t.Errorf("tail --no-follow missed history: %q", out)
	}
	if _, err := runCLI(t, sock, "", "tail", "ghost"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("tail(ghost) = %v", err)
	}
}

func TestCLI_ServeFlagsAndDaemonSubcommands(t *testing.T) {
	// serve's flag surface parses (we do NOT run the daemon here).
	out, err := runCLI(t, "/tmp/unused.sock", "", "serve", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--foreground", "--log-json", "--max-sessions", "--retention-ttl", "--ring-total", "--ws-port", "--takeover"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve --help missing %s", want)
		}
	}
	// daemon subcommands exist and document themselves.
	for _, sub := range []string{"install", "uninstall", "start", "stop", "status"} {
		if o, err := runCLI(t, "/tmp/unused.sock", "", "daemon", sub, "--help"); err != nil || !strings.Contains(o, "Usage:") {
			t.Errorf("daemon %s --help: %v", sub, err)
		}
	}
}

func TestCLI_ErrorsListJSON(t *testing.T) {
	raw := mustCLI(t, "/tmp/unused.sock", "errors", "list", "--json")
	var rows []struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("errors list --json: %v", err)
	}
	if len(rows) < 20 {
		t.Errorf("only %d codes listed", len(rows))
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Code, "E_") || r.Description == "" || r.Description == r.Code {
			t.Errorf("undocumented code row: %+v", r)
		}
	}
	// version --json carries the protocol version the daemon negotiates on.
	var v struct {
		BinaryVersion   string `json:"binary_version"`
		ProtocolVersion int    `json:"protocol_version"`
	}
	if err := json.Unmarshal([]byte(mustCLI(t, "/tmp/unused.sock", "version", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.ProtocolVersion != proto.ProtocolVersion || v.BinaryVersion == "" {
		t.Errorf("version --json = %+v", v)
	}
}

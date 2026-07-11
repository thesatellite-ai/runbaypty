package daemon

import (
	"strings"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// TestReplayCommand_OverTheWire — task-m8-lastcmd: a shell emitting OSC 133
// marks; REPLAY_COMMAND returns exactly the last command's output window.
func TestReplayCommand_OverTheWire(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Two marked commands; the SECOND is the replayable one.
	script := `printf '\033]133;C\007first-cmd-out\033]133;D;0\007'; ` +
		`printf '\033]133;C\007second-cmd-out\033]133;D;3\007'; sleep 300`
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatal(err)
	}

	// Poll: the marks land asynchronously with the read pump.
	var data []byte
	deadline := ctxT(t)
	for {
		data, _, _, err = c.LastCommandOutput(ctxT(t), id)
		if err == nil {
			if strings.Contains(string(data), "second-cmd-out") {
				break
			}
		} else if !errcodes.IsCode(err, errcodes.NotFound) {
			t.Fatal(err)
		}
		select {
		case <-deadline.Done():
			t.Fatalf("last command never captured (err %v, data %q)", err, data)
		default:
		}
	}
	if strings.Contains(string(data), "first-cmd-out") {
		t.Errorf("window bled into the previous command: %q", data)
	}
	// Session without marks → typed NotFound with the shell-integration hint.
	plain, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo nomarks; sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.LastCommandOutput(ctxT(t), plain); !errcodes.IsCode(err, errcodes.NotFound) {
		t.Errorf("expected E_NOT_FOUND for unmarked session, got %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
	_ = c.Kill(ctxT(t), plain, proto.SignalKILL)
}

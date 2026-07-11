// dev-server shows the original motivating use case: a long-running process
// (a dev server, a watcher, a `claude` session) that must OUTLIVE the tool
// that started it. Rebuild your app, quit your terminal, restart your editor
// — the dev server keeps running inside the daemon, and you reattach to the
// same process with its scrollback intact.
//
// This program is idempotent: run it once and it starts the named session;
// run it again and it just attaches to the already-running one. That is the
// pattern an app uses on every launch — "ensure my dev server is up, then
// show me its output."
//
// Run it twice to see the difference:
//
//	go run ./examples/dev-server            # starts "webdev", streams for 3s
//	go run ./examples/dev-server            # finds it already running, reattaches
//	go run ./examples/dev-server --stop     # tears it down
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// sessionName is the stable handle. A NAME (not an opaque id) is what makes
// "ensure it's running" idempotent across process restarts: the app knows
// the name, the daemon resolves it to whatever id is live.
const sessionName = "webdev"

// devServerScript stands in for `npm run dev` / `vite` / `air`: a process
// that runs forever and prints a heartbeat, the way a watcher does.
const devServerScript = `echo "dev server starting on :3000"; ` +
	`i=0; while :; do echo "[$(date +%T)] request $i handled"; i=$((i+1)); sleep 1; done`

func main() {
	stop := flag.Bool("stop", false, "stop the dev server instead of ensuring it")
	flag.Parse()
	if err := run(*stop); err != nil {
		fmt.Fprintln(os.Stderr, "dev-server:", err)
		os.Exit(1)
	}
}

func run(stop bool) error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	if stop {
		if err := c.Kill(ctx, sessionName, ""); err != nil {
			if errcodes.IsCode(err, errcodes.SessionNotFound) {
				fmt.Println("dev server was not running")
				return nil
			}
			return err
		}
		fmt.Println("dev server stopped")
		return nil
	}

	// Ensure-running: try to find it; spawn only if it's not there. The
	// name lookup is the whole trick — Info by name tells us if a previous
	// run (or a previous app launch) already started it.
	id, fresh, err := ensureRunning(ctx, c)
	if err != nil {
		return err
	}
	if fresh {
		fmt.Printf("started dev server %s (name %q)\n", id, sessionName)
	} else {
		fmt.Printf("dev server already running as %s — reattaching\n", id)
	}
	fmt.Println("streaming for 3s (the session keeps running after we detach)…")
	fmt.Println("──")

	// Attach and stream a slice of its output, then detach. Detaching does
	// NOT stop the session — that is the point.
	stream, err := c.Attach(ctx, id, nil, true /* read-only: just watching */)
	if err != nil {
		return err
	}
	streamFor(stream, 3*time.Second)

	fmt.Println("──")
	fmt.Printf("detached. `%s ls` shows it still running; run this example again to reattach.\n", constants.BinaryName)
	fmt.Printf("stop it with: go run ./examples/dev-server --stop\n")
	return nil
}

// ensureRunning returns the live session id for sessionName, spawning it if
// absent. fresh reports whether we started it this call.
func ensureRunning(ctx context.Context, c *client.Client) (id string, fresh bool, err error) {
	info, err := c.Info(ctx, sessionName)
	if err == nil {
		return info.ID, false, nil // already up
	}
	if !errcodes.IsCode(err, errcodes.SessionNotFound) {
		return "", false, err // a real error, not "not found"
	}
	// Not running — spawn it. linger defaults to true, so it survives every
	// client detaching; that is exactly what a dev server needs.
	id, _, err = c.Spawn(ctx, client.SpawnOpts{
		Cmd:  "/bin/sh",
		Args: []string{"-c", devServerScript},
		Name: sessionName,
		Cols: 120, Rows: 40,
	})
	if err != nil {
		// Race: another launcher spawned it between our Info and Spawn.
		if errcodes.IsCode(err, errcodes.NameTaken) {
			info, ierr := c.Info(ctx, sessionName)
			if ierr != nil {
				return "", false, ierr
			}
			return info.ID, false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

// streamFor copies a session's output for the given duration, then stops
// reading (without ending the session).
func streamFor(stream io.Reader, d time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(os.Stdout, stream)
	}()
	select {
	case <-done: // session ended on its own
	case <-time.After(d): // we've seen enough; detach by returning
	}
}

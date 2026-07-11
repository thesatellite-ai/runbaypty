// session-dashboard is a live, self-updating table of every session the
// daemon owns — the "mission control" view. It seeds from a LIST snapshot,
// then keeps itself current from the event stream, redrawing whenever
// anything changes. No polling: the daemon pushes every create, exit,
// rename, and attach.
//
// This is the read side of a process manager or an app's "running services"
// panel. It never spawns or kills anything; it observes.
//
// Run it, then create and kill sessions from another terminal to watch the
// table react:
//
//	go run ./examples/session-dashboard
//	# elsewhere: runbaypty run --name api -- sh -c 'sleep 300'
//	#            runbaypty kill api
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "session-dashboard:", err)
		os.Exit(1)
	}
}

func run() error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Subscribe to events BEFORE the initial LIST. If we listed first and
	// subscribed second, a session created in the gap would be missed by
	// both. Subscribe-then-list means every session is caught by one or the
	// other (and duplicates are harmless — the model is keyed by id).
	events, err := c.SubscribeEvents(ctx, "")
	if err != nil {
		return err
	}

	dash := &dashboard{c: c, rows: map[string]proto.SessionInfo{}}
	if err := dash.seed(ctx); err != nil {
		return err
	}
	dash.render()

	// Redraw on every event. Most events change what the table shows, so we
	// re-fetch the affected session's Info and repaint. A production UI would
	// debounce; a demo repaints eagerly so the causality is obvious.
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\ndashboard stopped")
			return nil
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("daemon closed the event stream")
			}
			dash.apply(ctx, ev)
			dash.render()
		}
	}
}

// dashboard holds the current view: a map of session id → its last-known
// Info, kept in sync by events.
type dashboard struct {
	c    *client.Client
	mu   sync.Mutex
	rows map[string]proto.SessionInfo
}

// seed fills the table from a one-shot LIST snapshot.
func (d *dashboard) seed(ctx context.Context) error {
	sessions, err := d.c.List(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range sessions {
		d.rows[s.ID] = s
	}
	return nil
}

// apply mutates the model for one event. An exited session is kept (with its
// new state) rather than dropped, so the table shows deaths — a supervisor
// wants to see what died, not have it silently vanish.
func (d *dashboard) apply(ctx context.Context, ev client.Event) {
	switch ev.Type {
	case proto.EventCreated, proto.EventRenamed, proto.EventAttached,
		proto.EventDetached, proto.EventResized, proto.EventMetaChanged, proto.EventExited:
		// Re-fetch the authoritative Info for the changed session. If it was
		// already reaped, drop it from the table.
		if info, err := d.c.Info(ctx, ev.SessionID); err == nil {
			d.mu.Lock()
			d.rows[ev.SessionID] = info
			d.mu.Unlock()
		} else {
			d.mu.Lock()
			delete(d.rows, ev.SessionID)
			d.mu.Unlock()
		}
	}
}

// render clears the screen and draws the table. Sorted by id (which is
// UUIDv7, so id order is creation order).
func (d *dashboard) render() {
	d.mu.Lock()
	rows := make([]proto.SessionInfo, 0, len(d.rows))
	for _, r := range d.rows {
		rows = append(rows, r)
	}
	d.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	// ANSI clear-screen + home. Kept literal-free via named constants.
	fmt.Print(ansiClear + ansiHome)
	fmt.Printf("runbaypty dashboard — %d session(s) — updated %s (ctrl-c to quit)\n\n",
		len(rows), time.Now().Format("15:04:05"))
	fmt.Printf("%-38s %-12s %-9s %-7s %-8s %s\n", "ID", "NAME", "STATE", "PID", "CLIENTS", "CMD")
	fmt.Println(strings.Repeat("─", 92))
	if len(rows) == 0 {
		fmt.Println("(no sessions — create one: runbaypty run -- <command>)")
	}
	for _, r := range rows {
		cmd := r.Cmd
		if len(r.Args) > 0 {
			cmd += " " + strings.Join(r.Args, " ")
		}
		if len(cmd) > 34 {
			cmd = cmd[:31] + "…"
		}
		fmt.Printf("%-38s %-12s %-9s %-7d %-8d %s\n", r.ID, truncate(r.Name, 12), r.State, r.Pid, r.Subscribers, cmd)
	}
}

// ANSI control sequences (closed set — named, not inlined).
const (
	ansiClear = "\033[2J"
	ansiHome  = "\033[H"
)

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

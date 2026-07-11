# dev-server

**Use case:** a dev server (or file watcher, or a `claude` session, or any long-running process) that must survive the tool that launched it. Rebuild your app, quit your terminal, restart your editor — the process keeps running and you reattach to it with its scrollback intact.

This is the problem runbaypty was born to solve.

## Run it

```sh
go run ./examples/dev-server          # starts "webdev", streams 3s of heartbeat
go run ./examples/dev-server          # finds it ALREADY running, reattaches
go run ./examples/dev-server --stop   # tears it down
```

Between the first and second run, notice the session id is *the same* — the second invocation didn't restart anything, it reattached to the process the first one started. `runbaypty ls` between runs shows it alive with nobody attached.

## The pattern: ensure-running by name

An app doesn't want "start a dev server." It wants "make sure the dev server is up, then show me its output" — on *every* launch, whether this is a cold boot or the fifth rebuild today. That's an idempotent operation keyed on a **name**:

```go
info, err := c.Info(ctx, "webdev")
if errcodes.IsCode(err, errcodes.SessionNotFound) {
    // not running — start it, keyed to the same name
    id, _, _ = c.Spawn(ctx, client.SpawnOpts{Cmd: "npm", Args: []string{"run", "dev"}, Name: "webdev"})
} else {
    id = info.ID   // already up — just use it
}
```

The name is the stable handle. Opaque `ses_…` ids change every spawn; the name `webdev` is what the app remembers across its own restarts. The daemon resolves the name to whatever session is currently live.

## Why the session outlives everything

Two properties make this work, both defaults:

- **The daemon owns the process, not your app.** `os/exec` would make the dev server a child of your app — kill the app, kill the server. Here the daemon is the parent, and the daemon is OS-managed (launchd/systemd), so it outlives every app rebuild.
- **`linger` is true by default.** A session survives every client detaching. A dev server with no one attached is normal — it's still serving requests; you just aren't watching. (Set `NoLinger` at spawn for the opposite: a session that dies when its last viewer leaves, useful for ephemeral runners.)

## The race, handled

Two app instances launching at once could both see "not found" and both try to spawn `webdev`. The second `Spawn` gets `E_NAME_TAKEN` — names are unique — and the example recovers by looking up the winner. Idempotent operations must handle their own races; this one does.

## Where this scales

- Wrap it in a supervisor that ensures *several* named services (api, web, worker) and you have a process manager that survives its own restarts.
- Give the app a browser UI over the [WebSocket transport](../browser-terminal/) and the dev server's output shows up in your app's own window — surviving the app's hot-reload.
- Add a [durable log](../record-and-export/) and you can scroll back through everything the server printed since it started, not just what's in the ring.

## Next

- [session-dashboard](../session-dashboard/) — a live view of all your named services at once
- [zero-downtime-upgrade](../zero-downtime-upgrade/) — even upgrading the daemon doesn't stop the dev server

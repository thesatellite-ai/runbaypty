# browser-terminal

**Use case:** a full, interactive terminal in a web browser — like [ttyd](https://github.com/tsl0922/ttyd) or [gotty](https://github.com/yudai/gotty), but the session it shows is a persistent runbaypty session. Close the tab, reopen it, and the shell is exactly where you left it, because the browser was never holding the terminal — the daemon was.

## Run it

Start the daemon with the WebSocket listener, then the helper:

```sh
bin/runbaypty serve --ws-port 8377 &
RUNBAYPTY_HOME=<daemon-home> go run ./examples/browser-terminal
# open the printed URL, e.g. http://127.0.0.1:9090/
```

```
spawned a demo shell session: ses_019f…

browser terminal (read-write) for session ses_019f…
open:  http://127.0.0.1:9090/
(ctrl-c to stop the web helper; the session keeps running)
```

Open the URL and you get a live shell in the browser. Type `echo browser-works-$((6*7))` and it prints `browser-works-42` — the keystrokes went from the browser to the daemon to the PTY, the shell evaluated them, and the output came back and rendered. (Verified exactly that way while building this example.)

Options: `--session <id|name>` to show an existing session instead of spawning a demo shell; `--read-only` to serve a view-only terminal; `--ws-port` / `--addr` to point at a different daemon port or bind address.

## Two processes, and why

There are two servers involved, and keeping them separate is the whole design:

```
browser ──HTTP──► this Go helper      (serves ONE html page, holds the token)
   │
   └──WebSocket──► runbaypty daemon    (the actual terminal stream)
```

The Go helper is deliberately dumb: it serves a single page and holds **no terminal state**. The moment the page loads, the browser opens its *own* WebSocket **straight to the daemon** and streams the terminal from there. The helper isn't in the data path — it can't be a bottleneck, and it can crash or restart without touching the session. This is the opposite of ttyd/gotty, where the web server *is* the PTY owner and killing it kills your shell.

## The token problem, and how the helper solves it

The daemon's WebSocket requires a token (localhost WebSockets can't be authenticated by file permissions — see [read-only-share](../read-only-share/)). But a browser **can't read the daemon's 0600 token file**. So the helper bridges exactly that gap and nothing more:

1. it reads the token from the daemon's home dir (it runs as your user, so it can),
2. it injects the token into the page it serves, replacing a placeholder:

```go
page := strings.Replace(pageHTML, configPlaceholder, string(cfgJSON), 1)
```

The token reaches the browser only inside the same-origin localhost page the helper delivers, never in a URL or a log. `--read-only` reads `token.ro` instead, and the served page physically can't type — the daemon enforces the scope regardless of what the page's JavaScript attempts.

## What the browser does — the same protocol as everyone else

The page's JavaScript is the [raw-protocol-node](../raw-protocol-node/) client, in a browser, wired to xterm.js. The same four-field frame, the same handshake:

1. **`HELLO`** with the injected token.
2. **`ATTACH`** to the session (read-only if the scope is read-only).
3. **`TAKE_WRITE`** if we hold a control token, so keystrokes reach the PTY.
4. **`OUTPUT`** frames → `term.write(payload)` — xterm.js renders them.
5. **keystrokes** → `term.onData` → `INPUT` frames.
6. **window resize** → `RESIZE` frames, so the remote PTY's dimensions match.

## Raw bytes + a real emulator = a faithful terminal

This is where runbaypty's byte-stream design pays off visibly. The daemon passes PTY output through **raw** — every escape sequence the program emitted is in the `OUTPUT` payload, untouched. xterm.js is a real terminal emulator, so it interprets those sequences: colors, cursor movement, full-screen apps like `vim`, `htop`, `top` all render exactly as they would in a native terminal.

```js
case Type.OUTPUT:
    term.write(payload); // the REAL escape codes, interpreted by a REAL emulator
```

Contrast a tool that models the terminal as a grid server-side: it can only show you what *its* emulator understood, and anything it doesn't parse is lost. runbaypty never parses the grid — it moves bytes — so the fidelity ceiling is xterm.js's, not the daemon's.

## Persistence is the differentiator

Because it's a real runbaypty session, everything else composes:

- **Close the tab, reopen it** — the shell is still running; the new page attaches and replays the scrollback from the ring, zero-gap ([reattach-zero-gap](../reattach-zero-gap/)).
- **Open the same URL in two tabs** — two live views of one session, each with its own pump, neither slowing the other ([slow-consumer](../slow-consumer/)).
- **Hand someone the `--read-only` URL** — they watch, they can't touch ([read-only-share](../read-only-share/)).
- **Upgrade the daemon** underneath the open tab — the session survives ([zero-downtime-upgrade](../zero-downtime-upgrade/)).

## A note on xterm.js and CSP

This page loads xterm.js from a CDN, which needs network on first load. That's fine for a locally-served helper page (it isn't a published artifact with a strict CSP). For a fully offline or locked-down deployment, vendor `xterm.min.js` and `xterm.min.css` next to the helper and serve them yourself — the terminal logic doesn't change.

## Next

- [raw-protocol-node](../raw-protocol-node/) — the exact protocol this page speaks, explained frame by frame
- [read-only-share](../read-only-share/) — the `--read-only` mode's scope enforcement
- [reattach-zero-gap](../reattach-zero-gap/) — why closing and reopening the tab loses nothing

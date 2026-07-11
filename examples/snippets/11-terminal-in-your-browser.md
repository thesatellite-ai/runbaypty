# 11. A full terminal in your browser (one command)

> A real xterm.js terminal plus a control panel for nearly the whole protocol, in your browser.

**What it shows:** runbaypty's loopback WebSocket transport, driving a live terminal UI. Guided (it opens a browser), so there is no headless script.

## Try it

One command builds an isolated daemon, serves the page, and opens it:

```sh
task -d examples/terminal-playground play
```

It prints a URL (defaults to `http://127.0.0.1:9098/`) and opens your browser there.

## What to do in the UI

- **Spawn** a session from the left panel (tick "shell integration" to get OSC 133 command blocks).
- **Take write** in the toolbar, then click the terminal and type: it is a real shell.
- Watch the right-hand panels update live: **Info** (session detail), **Events** (the lifecycle stream), **Watch** (add a regex, see matches), **Cmds** (per-command exit codes).
- Toggle **read-only**: the daemon then refuses any attempt to type.
- Drag the window: the PTY resizes.

Stop it with `task -d examples/terminal-playground stop`.

## What just happened

The browser spoke the runbaypty wire protocol directly to the daemon over a WebSocket, with a token the helper injected. A tiny Go helper served the page and handed over the token; it is never in the data path. See the full example, including the ~500-line dependency-free client, at [`examples/terminal-playground`](../terminal-playground/). For the minimal one-file version, see [`examples/browser-terminal`](../browser-terminal/).

# runbaypty snippets: a playground

Short, copy-paste demos that show off what runbaypty can do. Each one is tiny, self-contained, and safe to run: it spins up its own throwaway daemon on a private socket, does one cool thing, and cleans up after itself. Nothing here touches a real daemon you might already have running.

**Every snippet has its own step-by-step doc** (the `.md` files below) that walks each command by hand with what it does and what output to expect, so you can copy-paste and test one command at a time. Ten of them also have a fully automated script (`.sh`) you can just run and watch.

## The snippets

| # | Snippet | The "aha" | Run |
|---|---|---|---|
| 01 | [survive-a-crash](01-survive-a-crash.md) | Your reader can die; the session does not care | script |
| 02 | [outlive-your-terminal](02-outlive-your-terminal.md) | The dev server that outlives every rebuild | script |
| 03 | [two-windows-one-session](03-two-windows-one-session.md) | Type here, everyone sees it | script + 2 terminals |
| 04 | [hot-swap-the-daemon](04-hot-swap-the-daemon.md) | Replace the daemon binary without dropping a session | script |
| 05 | [ping-me-when-done](05-ping-me-when-done.md) | "Is it done?" without polling | script |
| 06 | [wait-for-ready](06-wait-for-ready.md) | Block until a server is ready, then move on | script |
| 07 | [per-command-blocks](07-per-command-blocks.md) | Per-command exit codes and output (OSC 133) | script |
| 08 | [agent-hands-you-the-keyboard](08-agent-hands-you-the-keyboard.md) | The agent/human handoff | script + 2 terminals |
| 09 | [record-and-replay](09-record-and-replay.md) | Record a session, replay it with real timing | script |
| 10 | [reprint-full-history](10-reprint-full-history.md) | Reprint a session's complete history | script |
| 11 | [terminal-in-your-browser](11-terminal-in-your-browser.md) | A full terminal in your browser, one command | guided (browser) |
| 12 | [read-only-share](12-read-only-share.md) | Share a session read-only | guided |
| 13 | [slow-reader-cant-stall-you](13-slow-reader-cant-stall-you.md) | A slow reader cannot stall you | Go example |
| 14 | [remote-as-local](14-remote-as-local.md) | Drive a remote daemon as if it were local | guided (ssh) |

## Two ways to use these

**Read a snippet's doc and copy-paste, command by command.** Open any `.md` above. Each one starts with a one-time setup block (a private scratch daemon), then walks each command with an explanation and the output you should see. This is the "test it by hand" path.

**Or just run the automated script and watch it narrate itself:**

```sh
# from the repo root, after: go build -o bin/runbaypty ./cmd/runbaypty
bash examples/snippets/01-survive-a-crash.sh

# or via Taskfile:
task -d examples/snippets --list        # the menu
task -d examples/snippets snip:04       # run one
task -d examples/snippets all           # run every automated snippet (01-10)
```

## Notes

- Snippets 01 through 10 have automated scripts (`examples/snippets/NN-name.sh`). Each runs an isolated daemon in `/tmp` and cleans it up on exit, so they never interfere with each other or with a real daemon.
- Snippets 11 through 14 are guided, because they need a browser, two terminals, or ssh. Their docs give the exact commands and link to the matching full example under [`examples/`](../).
- For the deeper, programmatic versions of these ideas, see the Go examples in the parent directory (each snippet's doc links to the relevant one).

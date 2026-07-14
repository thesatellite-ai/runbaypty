# sessions

Spawning sessions, sending input, reading output, and the single-writer model.

## Spawn

```sh
runbaypty run --name build -- make -j8
```

Flags on `run`:

- `--name <name>`: a unique handle you can use in place of the id.
- `--log <path>`: write the full output to a durable file (see the `history` skill).
- `--ring <bytes>`: size of the in-memory replay window (default 2 MiB).
- `--cwd <dir>`: working directory for the process.
- `--json`: print `{id, pid, name}` instead of just the id.

`run` prints the new session id on stdout, so capture it:

```sh
ID=$(runbaypty run --name build -- make -j8)
```

## Attach: an interactive terminal

```sh
runbaypty attach build
```

You get a full raw terminal into the session: type commands, run full-screen apps (vim, htop), see live output. Detach with `ctrl-\` (backslash); the session keeps running. Reattach any time and the scrollback replays.

`--read-only` attaches to watch only: you see output but cannot type, and you do not take the write lock from whoever is driving.

## Send input non-interactively (scripting)

Pipe stdin into `attach`:

```sh
printf 'cd /app\nnpm test\n' | runbaypty attach build
```

The piped bytes go to the session's stdin. Combine with a `timeout` to bound how long you read output back.

## The write lock (one writer at a time)

Exactly one client may send input to a session at a time. This prevents two writers from interleaving keystrokes into garbage.

- `attach` takes the write lock automatically.
- A second `attach` STEALS it (this is the agent/human handoff: a person can grab the keyboard from an agent mid-task, or vice versa).
- `attach --read-only` never takes it.
- If you send input without holding it, you get `E_NO_WRITE_LOCK` with a hint to take the lock.

## Manage a session

```sh
runbaypty ls                          # list all sessions
runbaypty info build --json           # full detail: pid, state, bytes, seq, subscribers
runbaypty rename build ci             # change the name (empty string clears it)
runbaypty resize build                # set cols/rows (last writer wins)
runbaypty meta merge build k=v a.b:=5 # merge JSON metadata (get/replace/unset too)
runbaypty kill build --signal TERM    # signal the whole process tree (TERM|KILL|INT|HUP)
```

## Lifecycle notes

- A session that exits lingers for a retention window (default 10 minutes) so a late client can still see the death and replay scrollback, then it is reaped.
- `run` without `--name` still works; refer to the session by its `ses_...` id.
- Spawn a shell (`-- /bin/sh -i`) when you want an ongoing session to send many commands to; spawn a bare command when you want a one-shot job whose exit code and output you will collect.

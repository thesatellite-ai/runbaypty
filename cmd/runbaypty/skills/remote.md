# remote

Reaching a daemon over the network: the WebSocket transport, scoped tokens, and driving a remote daemon over ssh.

## Two transports, one protocol

The daemon speaks the same wire protocol over two transports:

- **Unix socket** (default): same-host only. Authenticated by file permissions (mode 0600). If you can open the socket, you are allowed. No token.
- **Loopback WebSocket**: for browsers and cross-process clients. Enabled with `runbaypty serve --ws-port 8377`. Authenticated by a token, because localhost TCP cannot rely on file permissions.

## WebSocket tokens (scoped)

When the WebSocket listener is on, the daemon mints two tokens at boot into its home dir (`RUNBAYPTY_HOME`), mode 0600:

- `token`: control scope: the full protocol (spawn, input, kill, everything).
- `token.ro`: read-only scope: `LIST`, `INFO`, `ATTACH` (forced read-only), `DETACH`, `SUBSCRIBE_EVENTS`. Watch only.

A read-only token is enforced by the daemon: every control verb on it is refused with `E_READ_ONLY_SCOPE`, and a read-only credential can never yield a writable attach. So `token.ro` is safe to hand to someone who should watch but not touch. The token rides in the connection's HELLO frame, never in a URL.

The WebSocket URL is `ws://127.0.0.1:<port>/v1`. The listener binds loopback only.

## Drive a remote daemon over ssh (as if it were local)

Every runbaypty client dials a socket PATH; it has no notion of a host or port. So forwarding the remote daemon's Unix socket over ssh makes the local client drive the remote daemon with zero remote-side code:

```sh
# forward the remote socket to a local path (encrypted, over ssh):
ssh -N -L /tmp/rpty-remote.sock:/run/user/1000/runbaypty/runbaypty.sock you@host &

# now every client pointed at that path talks to the REMOTE daemon:
export RUNBAYPTY_SOCK=/tmp/rpty-remote.sock
runbaypty ls                 # lists the remote daemon's sessions
runbaypty run -- htop        # spawns htop on the remote machine
runbaypty attach <name>      # drive it from here
```

An `ssh -L` forward is a byte-for-byte forward of a Unix socket, which is indistinguishable from a local socket to the client. Nothing on the remote side needs porting.

Persistence composes with this: start a job remotely, let the tunnel drop (close your laptop), re-establish it later, and reattach to the still-running remote session. ssh is only how you reach the daemon; it does not hold your processes.

## Security note

A forwarded Unix socket has your user's file permissions locally, which grant full control scope. Put it somewhere only you can reach (for example `$XDG_RUNTIME_DIR`), not a world-readable `/tmp`, if other users share the machine. The ssh channel itself is encrypted; the exposure is the local socket's permissions.

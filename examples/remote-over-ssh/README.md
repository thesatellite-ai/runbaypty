# remote-over-ssh

**Use case:** drive a daemon running on another machine — a build server, a cloud box, a lab machine — as if it were on your laptop. No remote agent to install, no daemon configuration, no runbaypty-specific networking. Just ssh.

## Run it

Point `RUNBAYPTY_SOCK` at an ssh-forwarded socket (set up the forward first — see the next section), then run it exactly as you would against a local daemon:

```sh
RUNBAYPTY_SOCK=/tmp/rpty-remote.sock go run ./examples/remote-over-ssh
```

```
connecting to the daemon at /tmp/rpty-remote.sock
(in production this path is an ssh -L forward — see the README)

the daemon owns 3 session(s)

spawning `hostname && uname -sm` on the daemon's host:
  host:   build-server-01
  system: Linux x86_64
  (exit 0)
```

`hostname` prints the **remote** machine's name and `uname` its OS — the process ran over there, while you drove it from here. Point the same command at a local socket (`RUNBAYPTY_SOCK=/tmp/rpty-ex.sock`) instead and it's unremarkable: identical code, your own host. That sameness is the point.

## The one flag that does everything

Modern OpenSSH forwards Unix domain sockets, not just TCP ports:

```sh
ssh -N -L /tmp/rpty-remote.sock:/run/user/1000/runbaypty/runbaypty.sock you@build-server
```

- `-L /tmp/rpty-remote.sock:/path/to/remote.sock` — create a local socket at `/tmp/rpty-remote.sock`; every connection to it is forwarded, encrypted, to the remote daemon's socket.
- `-N` — don't run a remote command; just hold the tunnel open.

Find the remote daemon's socket path with `runbaypty --help` or `echo $RUNBAYPTY_SOCK` on the remote host (default is under its runbaypty home). Then, locally:

```sh
export RUNBAYPTY_SOCK=/tmp/rpty-remote.sock
runbaypty ls                 # lists the REMOTE daemon's sessions
runbaypty run -- htop        # spawns htop on the REMOTE machine
go run ./examples/remote-over-ssh
```

Everything works against the remote daemon, because to every client the forwarded socket is just a socket.

## Why this needs zero remote-side code

Look at the example: there is no ssh in it, no host, no port, no "remote" anything. The entire program is:

```go
sock, _ := constants.SocketPath()   // = RUNBAYPTY_SOCK
c, _ := client.Dial(sock)           // dials a socket PATH
c.List(ctx); c.Spawn(...); c.Attach(...)
```

Every runbaypty client — the SDK, the CLI, the raw-protocol examples — connects to a socket *path* and speaks the wire protocol over it. None of them interpret a host or a port; none of them have a notion of "remote." So the transport underneath the path is free to be anything that delivers bytes in order: a local Unix socket, an ssh forward, a `socat` bridge, a VPN'd socket. **ssh -L is a byte-for-byte forward of a Unix socket** — which is exactly what a local Unix socket is — so the client cannot tell the difference, and there's nothing to port.

This example was verified by putting a transparent Unix-socket proxy between the client and the daemon (precisely what `ssh -L` is, minus the encryption) and confirming the SDK worked through it unchanged. That's the proof: if a dumb byte-splicing proxy is indistinguishable from a direct connection, so is ssh.

## Persistence composes with remoting

The remote daemon is still a runbaypty daemon, so everything else in these examples still holds — over the tunnel:

- Start a long-running job on the remote host, close your laptop (the ssh tunnel drops), reopen it, re-establish the tunnel, and [reattach zero-gap](../reattach-zero-gap/) to the still-running remote session.
- Use [`client.Follow`](../follow-resilient/) so the reader survives the tunnel dropping and re-establishing — the same resilience that survives a [daemon upgrade](../zero-downtime-upgrade/) survives a flaky ssh connection.
- The remote session outlives your ssh session entirely. ssh is just how you *reach* the daemon; it isn't holding your processes. That's the difference from running `ssh you@host htop` directly, where closing ssh kills htop.

## Sharing a remote session read-only

Combine this with [read-only-share](../read-only-share/): forward the remote socket, hand a colleague the remote daemon's `token.ro` over the WebSocket transport, and they can watch a session on your build server without the ability to touch it. The transport (ssh) and the authorization (token scope) are independent layers that stack.

## Security note

The forwarded Unix socket on your local machine has your user's file permissions — anyone who can reach `/tmp/rpty-remote.sock` gets the daemon's full control scope (UDS trusts file permissions). Put the forwarded socket somewhere only you can access (e.g. `$XDG_RUNTIME_DIR`), not a world-readable `/tmp`, if other users share your machine. The ssh channel itself is encrypted; the exposure is purely the local socket's permissions.

## Next

- [reattach-zero-gap](../reattach-zero-gap/) — reconnect to a remote session after the tunnel drops
- [follow-resilient](../follow-resilient/) — a reader that rides through the tunnel dropping and reconnecting
- [read-only-share](../read-only-share/) — hand out watch-only access to a remote session

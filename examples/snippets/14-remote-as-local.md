# 14. Drive a remote daemon as if it were local

> Point a local client at an ssh-forwarded socket and manage a daemon on another machine, with zero remote-side code.

**What it shows:** every runbaypty client dials a socket path, so the transport under it can be anything that delivers bytes in order, including an `ssh -L` forward. Guided (it needs a real remote host over ssh).

## Try it

### Step 1: forward the remote daemon's socket over ssh

Find the remote daemon's socket path (its `RUNBAYPTY_SOCK`, default under its home), then:

```sh
ssh -N -L /tmp/rpty-remote.sock:/run/user/1000/runbaypty/runbaypty.sock you@build-server &
```

- `-L /tmp/rpty-remote.sock:/path/to/remote.sock` creates a local socket that forwards, encrypted, to the remote daemon's socket.
- `-N` just holds the tunnel open (no remote command).

### Step 2: drive the remote daemon as if it were local

```sh
export RUNBAYPTY_SOCK=/tmp/rpty-remote.sock
bin/runbaypty ls                 # lists the REMOTE daemon's sessions
bin/runbaypty run -- htop        # spawns htop on the REMOTE machine
bin/runbaypty attach <name>      # drive it from here
```

## What just happened

There is no ssh, host, or port anywhere in runbaypty. The client only ever speaks to a socket path, so an `ssh -L` forward (which is a byte-for-byte forward of a Unix socket) is indistinguishable from a local socket. Nothing on the remote side needs porting.

And persistence composes with remoting: start a job on the remote host, close your laptop (the tunnel drops), reopen it, re-establish the tunnel, and reattach to the still-running remote session. ssh is only how you *reach* the daemon; it is not holding your processes. See the [remote-over-ssh](../remote-over-ssh/) example, which was verified through a transparent Unix-socket proxy (exactly what `ssh -L` provides).

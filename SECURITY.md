# Security Policy

## Supported versions

runbaypty is pre-alpha: only the latest `main` is supported.

## Security model (what to test against)

- The Unix domain socket and the lock/token files are mode `0600` — filesystem permissions are the local authentication boundary. If you can open the socket, the OS has already decided you're allowed.
- The WebSocket listener binds loopback (`127.0.0.1`) only. Its tokens are minted per daemon boot, compared in constant time, scoped (control vs read-only), and never appear in URLs or in the discovery file.
- The daemon runs unprivileged as the user; it never listens on non-loopback TCP.
- The daemon passes PTY bytes through raw — it does no terminal emulation and executes only the commands a client explicitly spawns.

## Reporting a vulnerability

Please **do not** open a public issue for security reports. Use GitHub's private vulnerability reporting on this repository (**Security → Report a vulnerability**). You'll get an acknowledgement within a week. Coordinated disclosure is appreciated, and credit is given unless you'd prefer otherwise.

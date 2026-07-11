# Contributing to runbaypty

Thanks for your interest! runbaypty is pre-alpha and moving fast; the wire protocol is the stable contract.

## Ground rules

- **The protocol is additive-only.** New frame types and JSON fields may be added; existing values and meanings never change. `pkg/proto/protodoc_test.go` fails if the internal protocol reference (docsi/PROTOCOL.md, present on maintainer machines) drifts from the code — update both together.
- **The daemon stays policy-free.** No database, recipes, auto-restart, panes, or screen-grid emulation in the daemon — that surface belongs to clients. PRs adding policy to the daemon will be redirected to the SDK/CLI layer.
- **Every behavior ships with a test.** `go test -race ./...` must stay green; new goroutines answer to goleak; anything parsing bytes gets a fuzz seed.

## Dev loop

```sh
task build        # bin/runbaypty
task test         # unit + integration, -race
task lint         # gofmt + go vet (CI also runs golangci-lint)
task run-daemon   # foreground daemon for manual poking
task bench        # firehose + latency numbers (update BENCH.md if they move)
```

Point `RUNBAYPTY_HOME` / `RUNBAYPTY_SOCK` at a scratch dir for an isolated daemon.

## Pull requests

1. Fork, branch from `main`.
2. Keep PRs focused; note protocol-affecting changes prominently.
3. Note any wire change prominently in the PR (additive-only; the maintainer-side drift test tracks the protocol reference).
4. `go test -race ./...` + `golangci-lint run` clean.

## Reporting bugs

Use the issue templates. For anything byte-stream-related, the magic words are: expected seq, observed seq — the sequence axis makes most bugs provable.

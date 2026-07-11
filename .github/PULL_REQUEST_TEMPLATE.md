## What

## Why

## Protocol impact
- [ ] No wire change
- [ ] Additive wire change (PROTOCOL.md updated — the drift test enforces it)

## Checklist
- [ ] `go test -race ./...` green
- [ ] `golangci-lint run` clean
- [ ] New goroutines have exit conditions (goleak passes)
- [ ] Daemon stays policy-free

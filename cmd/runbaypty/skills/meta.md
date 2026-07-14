# meta

Attach arbitrary JSON metadata to a session and update it safely. This is how an agent records what a session is, tracks live state, coordinates with other agents, and finds sessions later.

## The model

Every session has one JSON document called its `meta`. It is client-owned: the daemon stores it and never acts on it. You merge fields into it by default, so two writers touching different fields never clobber each other. It survives a daemon upgrade, and it lives exactly as long as the session (plus its retention window).

## Set it: the value grammar

The operator decides the type, so there is no guessing:

```sh
# key=value is ALWAYS a string; key:=value is parsed as JSON; dots nest the key
runbaypty meta merge build \
  task.id:=5 \
  task.name=deploy \
  tags:='["ci","fast"]' \
  budget:=900 \
  note='multi word string'
```

That merges `{"task":{"id":5,"name":"deploy"},"tags":["ci","fast"],"budget":900,"note":"multi word string"}` into the document. Only the fields you name change; everything else stays.

Rules to remember:

- `name=deploy` stores the string `"deploy"`. `id=007` stays the string `"007"` (no number coercion).
- `count:=5` stores the number 5. `active:=true` stores a bool. `tags:='["a","b"]'` stores the whole array as one value (it is not split per element).
- Invalid JSON after `:=` is a loud error, never a silent fallback to string. To store the literal word "blocked", use `state=blocked`, not `state:=blocked`.
- A literal dot in a key is escaped: `foo\.bar=1`.

## Real JSON without quoting hell

Pipe a file or stdin instead of fighting the shell:

```sh
echo '{"task":{"id":5}}' | runbaypty meta merge build --json -
runbaypty meta replace build --json @meta.json
```

## The verbs

```sh
runbaypty meta get build                 # print the JSON document
runbaypty meta merge build k=v a.b:=5    # merge fields in (the default, safe under concurrency)
runbaypty meta replace build only=this   # swap the whole document
runbaypty meta unset build task.id       # delete a key (dotted paths ok)
runbaypty meta incr build tokens=200     # atomically add to a numeric field (counters)
```

`incr` is the one thing merge cannot do: `meta incr build tokens=200 task.retries=1` adds to the existing numbers (a missing field starts at 0). Use it for token counters, retry counts, and progress that many writers bump.

## Find sessions by meta

Filtering is client-side (the daemon keeps no index):

```sh
runbaypty ls --filter env=staging                     # sessions whose meta.env == staging
runbaypty ls --filter env=staging --filter team=infra # repeatable flags are AND
runbaypty ls --filter task.name=deploy                # dotted keys work
```

## Concurrency: compare-and-swap

Merge already makes disjoint-field writes safe. When you need read-modify-write on the whole document to be safe against a racing writer, use the version:

```sh
runbaypty info build --json    # shows meta_version
runbaypty meta merge build status=done --if-version 12   # fails with E_META_CONFLICT if it moved
```

## React to changes

The `meta-changed` event fires on every write and carries `data.meta_keys` (the top-level keys touched) and `data.meta_version` (the new version), so a watcher can react without re-fetching. Treat meta as shared state you re-read, not as a reliable message queue: events are best-effort.

## Reserved namespace

Keys under `rpty` (a top-level `rpty` key or anything starting with `rpty.`) are reserved for the daemon and rejected with `E_RESERVED_META_KEY`. Use your own keys.

## What agents put here

- Provenance: `agent_id`, `model`, `run_id`, `parent`, `correlation_id`.
- Live progress (the blackboard): `status`, `step`, `progress`, updated as you work.
- Budget and counters: `budget_usd`, `tokens_used` (via `incr`), `retries`.
- Ownership and handoff: `owner`, `claimed_by`, `priority`.
- Fleet tags for `ls --filter`: `env`, `team`, `tags`.
- Replay context: `git_sha`, and checkpoints mapping a milestone to a seq position.

## Limits

The merged document is capped at 64 KiB. Meta is not durable past the session: if you need it to outlive the session, store it yourself. The SDK equivalent is `SpawnOpts.Annotations` at spawn and `SetMetaJSON(ctx, id, patch, SetMetaOpts{Mode, IfVersion})` after.

# Console logging

ezllm writes **one line per record to stdout** and nothing else. It has no log
file, no rotation and no logger library: `log/slog` plus `internal/logx`, which
is a `slog.Handler` that renders one line. What to do with stdout is the
platform's job.

The ledger is the record of a request. The console is what an operator reads
while something is wrong. They are deliberately different things:

- **ledger** (`GET /admin/requests`, the SQLite `calls` table) — one row per
  request, immutable, queryable, the billing truth.
- **console** — the story around that row: which hop was skipped and why, what
  a refusal taught us, what the clamp rewrote.

## Reading it

```sh
docker logs ezllm --tail 100          # live box
docker logs -f ezllm | grep -i 429    # follow one symptom
docker logs ezllm 2>&1 | grep 'ERROR'
```

`docker logs` without `-t` is not a terminal, so the default `auto` colour
stays off and the lines stay greppable. The binary behaves the same way: under
systemd it is `journalctl --user -u ezllm -f` (or `-u ezllm` for a system unit),
and launchd/Task Scheduler redirect stdout to a file — see
[install-binary.md](install-binary.md) §6. None of it needs anything from ezllm
itself.

**No log file, deliberately.** An in-process file writer would buy rotation,
locking, permissions and a disk-full failure mode in exchange for duplicating
state the platform already captures. If a durable local copy is ever needed,
redirect stdout at deploy time and let `logrotate` handle it — not the gateway.

## The shape of a line

```
2026-10-24 08:01:15.254 INFO  request method=POST path=/v1/chat/completions status=200 ms=25709 bytes=31568 client=melissa account=command-code model=meta/muse-spark-1.3-contributor surface=chat alias=daily trail="opencode-go/muse-spark — 429 rate_limited"
```

The access line is written by the `accessLog` middleware for **every** request,
auth'd or not. `alias` and `trail` appear only when they exist: a direct route
has no combo name, and a request whose first candidate served has no trail. The
trail is the point — a hop that failed over writes no ledger row of its own, so
`200` alone would hide a dead provider.

`EZLLM_LOG_FORMAT=json` switches the whole log to one JSON object per line for a
collector. The console format is the default because the first tool is
`docker logs`.

### The boot line

The process states what the log will do before anything else happens:

```
2026-10-24 09:07:07.823 INFO  console logging log_level=info format=console color=false
```

The key is `log_level`, not `level`: slog already emits `level` for the record
itself, and in JSON a second `level` would be a duplicate key whose value
depends on which parser reads it. The record's level is `INFO` while
`log_level` is what `EZLLM_LOG_LEVEL` asked for — two different facts.

The line is an `INFO` record, so at `warn`/`error` it is not shown. That is the
levels doing their job, not a bug: at those levels the operator has already
asked for silence about startup.

## Levels

| level | what belongs there | volume |
|---|---|---|
| `debug` | per-hop detail an operator turns on to chase one incident: auth refusals, a clamp that matched no catalog row, stream teardown | off by default; ~1.3 µs per line when on |
| `info` | state changes that outlive the request: boot, hop parked, clamp applied, vocabulary learned, wire recorded, budget reached, access lines | ~62/h on Calypso's box, 571 in the busiest hour |
| `warn` | something degraded but survived: a hop failed and failed over, a store write lost, rules did not reload | rare |
| `error` | the gateway could not do its job: admin store error, background job still running at shutdown | rare |

`EZLLM_LOG_LEVEL=debug` is the switch an operator reaches for. It must be worth
turning on: before the logging pass, the binary contained exactly **one**
`Debug` call, so setting it changed almost nothing.

### What is deliberately not logged

- **Never** a bearer token, provider key, `Authorization` header, prompt or
  response body, or a connection string. Refusals name the path and the reason,
  not the credential.
- No request id. There is no cross-service correlation to join on, and it would
  thread a value through every path for nothing.
- No per-request line at `debug` in addition to the `info` access line. Two
  lines per request is noise at double the cost; the access line is enriched
  instead.

## The performance rule

Measured on this box (`internal/logx`, `go test -bench`):

| case | cost |
|---|---|
| filtered-out `Debug`, cheap arguments | **17.7 ns, 0 allocs** |
| filtered-out `Debug`, one computed argument (`strings.Join` of 8) | **255.8 ns, 368 B, 2 allocs** |
| `Logger.Enabled` check alone | **7.1 ns** |
| a line actually emitted (`debug` or `info`) | ~1.3–1.5 µs, ~300 B |

Against a dispatch baseline of ~300 µs, a filtered `Debug` is 0.006% of a
request. Logging is therefore free **as long as the arguments are cheap**.

**The rule:** `slog` evaluates arguments *before* it checks the level, so any
`Debug` whose arguments cost more than a field read — a `strings.Join`, a
`sprintf`, a slice walk — must sit behind an explicit gate:

```go
if log.Enabled(ctx, slog.LevelDebug) {
    log.Debug("request", "trail", buildTrail(attempts))
}
```

Field reads (`rt.Account.Namespace`, `res.Status`) need no gate. Nothing on the
happy path builds a string it does not log: the access line's `trail` is
formatted only when `len(res.Attempts) > 0`.

## When to add a line

Add one when **an operator could not otherwise explain** what the gateway did:

| event | level | why it is not in the ledger |
|---|---|---|
| hop skipped (parked, budget, context too small) | info | the request never left the box; there is no row for it |
| client cancelled (no hop charged) | info | the row records `499`, but not that the hop was left untouched |
| reasoning clamp applied / field dropped | info | the ledger has the status, not the rewrite |
| reasoning vocabulary learned / not kept | info / debug | a per-model fact about the future |
| wire recorded after a mismatch probe | info | it reroutes every later request |
| client budget reached | info, once per window | the 429s are rows; the cause is not |
| rules refresh fails | warn | the last successfully loaded rules remain active |
| Gemini Web rotated cookie cannot be saved | warn | the next request retries persistence; credentials are never logged |
| auth refused (missing/invalid/expired/role) | debug | refused before dispatch: no row, and a retry loop must not flood the console |

Each of these has a test asserting the line appears at the right level and that
the level filters it out when it should not.

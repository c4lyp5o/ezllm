# Logging: decisions and rules

How ezllm's console log is built, and the rules for adding to it. Reading the log:
[docs/logging.md](../logging.md). Code: `internal/logx`.

## Shape

- **stdout only.** One line per record, no log file, no rotation and no logger library:
  `log/slog` plus `internal/logx`, a `slog.Handler` that renders one line. A file writer
  would bring rotation, locking, permissions and a disk-full failure mode, to duplicate
  what the platform already captures. Need a local copy? Redirect stdout at deploy time
  and let `logrotate` handle it.
- **The ledger and the console are different things.** The ledger (`GET /admin/requests`,
  the `calls` table) is one immutable row per request, the billing truth. The console is
  the story around that row: which hop was skipped and why, what a refusal taught, what a
  clamp rewrote.
- **The access line** is written by the `accessLog` middleware for every request, signed
  in or not. `trail` exists because a hop that failed over writes no ledger row of its
  own, so `200` alone would hide a dead provider.
- **The boot line's key is `log_level`, not `level`.** slog already emits `level` for the
  record, and in JSON a second `level` would be a duplicate key. The line is `INFO`, so at
  `warn` or `error` it is not shown, as asked.

## Levels

| level | what belongs there | volume |
|---|---|---|
| `debug` | per-hop detail an operator turns on to chase one incident: auth refusals, a clamp that matched no catalog row, stream teardown | off by default; ~1.3 µs per line when on |
| `info` | state changes that outlive the request: boot, hop parked, clamp applied, vocabulary learned, wire recorded, budget reached, access lines | ~62/h on one busy instance, 571 in the busiest hour |
| `warn` | something degraded but survived: a hop failed and failed over, a store write lost, rules did not reload | rare |
| `error` | the gateway could not do its job: admin store error, background job still running at shutdown | rare |

## What is never logged

- **Never** a bearer token, provider key, `Authorization` header, prompt or
  response body, or a connection string. Refusals name the path and the reason,
  not the credential.
- No request id. There is no cross-service correlation to join on, and it would
  thread a value through every path for nothing.
- No per-request line at `debug` in addition to the `info` access line. Two
  lines per request is noise at double the cost; the access line is enriched
  instead.

## The performance rule

Measured (`internal/logx`, `go test -bench`):

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
| subscription bridge refused requests, queue full (bridge, limits, totals) | warn, at most once per 10 s per bridge | the 429 `queue_full` rows exist, but not that the bridge itself is saturated; the line counts the refusals it did not log |
| notification push failed (channel, kind, reason) | warn | the bell row exists; the push outcome is the channel's status in Settings, and the reason never carries the channel's secrets |
| auth refused (missing/invalid/expired/role) | debug | refused before dispatch: no row, and a retry loop must not flood the console |

Each of these has a test asserting the line appears at the right level and that
the level filters it out when it should not.

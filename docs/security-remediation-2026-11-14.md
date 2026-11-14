# Security remediation — v1.14.1

Follow-up to the [whole-application audit](security-audit-2026-11-14.md).
The application fixes are implemented and tested. The host port configuration
has been changed to the operator's selected localhost binding and validated.
The running service has not been recreated or upgraded by this change.

## Resolution by finding

| Finding | Source/configuration resolution | Runtime rollout |
| --- | --- | --- |
| SEC-01 | Bounded global/per-client inference admission and retained input-memory reservations | Requires new application image |
| SEC-02 | Provider Set-Cookie, Set-Cookie2 and Clear-Site-Data headers removed before client responses | Requires new application image |
| SEC-03 | Authentication/access/panic paths redact full credentials, including encoded slash tokens; panic payloads are omitted | Requires new application image |
| SEC-04 | Pending Codex state bound to authenticated caller identity and canonical history; native ID collisions cannot overwrite another turn | Requires new application image |
| SEC-05 | Example and local deployment Compose files bind host port 20129 to 127.0.0.1 | Prepared; requires container recreation |
| SEC-06 | Go 1.27.4 pin and reachable-vulnerability CI gate | Source/release builds patched; observed existing container was already on Go 1.27.4 |
| SEC-07 | Settings writes reuse the shared bounded/deadlined admin decoder while retaining their 32 KiB cap | Requires new application image |

No real client credentials were rotated, no vendor sessions were updated and no
paid inference requests were made for these fixes. Historical log exposure is
not erased by redacting future logs; the audit did not establish an actual leaked
production credential that could be rotated without coordinating clients.

## Request admission

`admission.go` `admit` runs after authentication and before any inference body is
read. The following configuration is always bounded; zero is not an opt-out:

```yaml
max_body_mib: 32
request_limits:
  global_inflight: 32
  per_client_inflight: 8
  body_memory_mib: 256
```

All values must be positive and no greater than 4096. Per-client concurrency may
not exceed the global limit; the memory budget must cover one allowed body.
These are configuration-YAML values, not dashboard overrides or env flags.
The 256 MiB default covers eight simultaneous full-size 32 MiB body reads;
completed small reads relinquish unused reservation so more requests can run.

Admission reserves the full request cap before reading, including for unknown
length/chunked input. It then retains the actual input byte charge through
routing, upstream work and streaming. Completion, errors, cancellation and panic
release the reservation and remove idle per-client counters. Excess requests
receive 429 plus `Retry-After: 1` without queuing or reading another body. HTTP/1
rejections close instead of blocking on a stalled-body drain.

The memory budget is an input-byte budget, not a process RSS ceiling: decoded
JSON, transformed copies, response buffers, SQLite and CLI children use memory
too. Tune it against the deployment's measured resources and actual concurrency.
CLI process limits remain independent. SSE responses can still run longer than
the request-body read budget.

## Codex caller boundary

`proxy/caller.go` `WithCaller` records a credential-hash-derived identity after
successful gateway authentication. Dashboard sessions and client keys occupy
separate identity namespaces. Neither display names nor caller-supplied fields
establish ownership.

`copyClientHeaders` strips `X-Ezllm-Caller`. `stampCaller` also removes configured
spoofs and stamps the trusted context value only for the registered loopback
bridge origin with its matching credential. Remote providers receive no caller
identity. The production bridge requires this metadata for tool-bearing requests;
its ordinary shared-secret authentication still runs first.

`codexbridge/continuation.go` derives a canonical hash chain from actual caller
messages, preserving images, selected tool names/IDs and precise tool arguments.
Equivalent text-block representation and cache-control metadata normalize.
A pending turn stores hashes rather than a second copy of caller history.
The complete prior history and emitted assistant tool-call message must match
before a child can resume. Sequential tool batches advance the chain.

Foreign caller IDs are refused with 403 before the original parked turn is
removed. Changed or truncated history uses a fresh replay without exposing the
old native conversation. A recycled native call ID receives a client-facing
alias and remains mapped to its own native RPC reply. Existing bounded parking,
expiry and shutdown cleanup still apply.

Empty template parents remain configuration-only and can share cache affinity.
Caller identity and history hashes are not put in prompts, model inputs or CLI
arguments. Approval policy remains `never`; writes and commands still run in
Hermes or the calling harness.

## Regression evidence

Permanent tests exercise the real HTTP/gateway path with local synthetic
upstreams and the existing fake CLI. They cover:

- Global, per-client and byte-budget admission before body reads; chunked bodies;
  reservation retention and cleanup after cancellation, malformed input and panic.
- Cookie/origin-state filtering on public and dashboard inference, with a real
  HTTP cookie jar; provider rate-limit headers remain available.
- Disabled path credentials in debug logs, encoded slash credentials, and
  credential-bearing panic payloads.
- Authenticated gateway-to-Codex identity propagation, external header/JSON
  spoof rejection, no identity leakage to unregistered upstreams, wrong-owner
  rejection without consuming the rightful turn, full-history/marker matching,
  sequential native tool batches and recycled call IDs.
- The settings timeout over a real socket, positive/finite configuration bounds,
  and existing auth, role, cache, streaming and lifecycle assertions.

`python3 tests/security_audit.py` now runs these permanent rejection/redaction
regressions. It no longer writes temporary tests or treats successful exploitation
as a passing audit. Earlier positive vulnerable-behavior probes remain available
in audit commit `401dfbc` for historical comparison.

Validation includes `make test-short`, focused `go test -short -race`, `go vet`,
the 54 updater tests, static compilation and the pinned CI vulnerability command:

```bash
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

The patched toolchain reports no reachable vulnerabilities. The uncalled
OpenPGP-deprecation module record remains an advisory, as documented in the audit.
No browser UI sources were changed, so the embedded dashboard needs no rebuild.

## Deployment boundary

The observed running service was ezllm 1.8.1. Deploying v1.14.1 also carries the
previous branch changes, including bridge cache features; this is a service
upgrade in addition to a port-binding change. The prepared Compose mapping is
`127.0.0.1:20129:20129`. Traefik continues to reach the container through its
existing Docker network; local clients retain the host port.

Private Compose files and their backups are excluded from the Docker build
context alongside live config, secrets and data. Local test builds and disposable
smoke containers do not replace the running service. Before production recreation,
verify the new image, retain a consistent database backup and the current image,
then check localhost health, authentication and proxy routing after recreation.
The current runtime status must be rechecked after that rollout; a source fix is
not evidence that an older running process has changed.

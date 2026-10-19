# Single-Parse Dispatch — Design (OPT-1)

Status: **implemented 2026-10-15**, measured results below. Locked DECISIONS
follow the results section.
Supersedes: nothing (additive). Companion to `docs/perf-baseline-m1.md` §6,
which recommended exactly this work.

## Problem (measured, M1)

A request body is fully JSON-decoded up to **5 times** before dispatch:

1. `extractModel` (server.go) — struct decode for `model`
2. `IsStream` (proxy.go, per attempt) — struct decode for `stream`
3. `SwapModel` (proxy.go, per hop) — map decode + re-marshal
4. `compress.Apply` (compress.go) — map decode
5. `captureShape` (shape.go) — map decode

Phase 2 is the only router phase that scales with payload
(303 µs @64 KiB → 1.70 ms @1 MiB; ~1.5 ms/MiB is JSON parse), and the
dominant client sends ~0.7–1 MB bodies. Every one of those consumers wants
the same two facts (`model`, `stream`) or the same map.

## Design

**Decode the body exactly once per request into a shared `BodyDoc`; every
consumer reads facts from it; the hop's model swap mutates + marshals the
shared map instead of re-decoding.**

- New package `internal/jsonbody`: `BodyDoc` = raw bytes + one
  `map[string]any` decoded with `json.Decoder.UseNumber()` + trailing-garbage
  check (`dec.More()`), mirroring the old `SwapModel` decode exactly.
- `internal/server.inference()` opens the doc once after the body read and
  threads it: `extractModelDoc` → `applyCompression(..., doc)` →
  `captureShapeDoc` → `ForwardCandidatesDoc(..., doc)` → per-hop
  `prepareHopBody(body, rt, doc)`.
- **doc is a pure optimization hint.** Every entry point keeps its byte-based
  signature and falls back to the legacy parse path when doc is nil — the
  ~40 existing test call sites, `admin.go` combo-test, `services.go` probes
  and the multipart STT path are untouched and byte-identical.
- Native hop: `doc.WithModel(hopModel)` splices the hop model into the
  client's own bytes (`jsonbody.SpliceModel`, decision 6) and falls back to
  `json.Marshal` of the shared map where a splice cannot normalize the body.
  Translated hops still rebuild the body through `translate.*` from the raw
  bytes (unchanged). Multipart unchanged.
- Byte fidelity: `SwapModel` makes the same splice-or-re-marshal choice, so the
  doc and legacy paths stay byte-identical for equal input.

## Parity rules (what "unchanged behavior" means here)

- `extractModel` semantics preserved exactly: absent or `null` model → `("", nil)`;
  string → trimmed; ANYTHING else (number, bool, object, garbage, trailing
  junk) → `400 "body is not a JSON object"`. (Old struct-decode wrapped every
  unmarshal failure into that same constant string — including `{"model":123}`.)
- `IsStream` semantics preserved: only a real JSON `true` streams; everything
  else false. The flag is computed ONCE from the client body and used for every
  attempt (swap/translation never change `stream`).
- Duplicate JSON keys: last-wins, same as both old decoders (struct + map).
- `UseNumber` everywhere in the shared map. NOTE: `captureShape` previously
  decoded WITHOUT UseNumber, so `prefix_sha` for bodies containing
  exponent-notation numbers or >2^53 ints will hash the LITERAL now
  (`1e10` stays `1e10`) instead of the float64 re-render (`1e+10`). This is a
  research-telemetry normalization improvement, documented here as intended;
  no routing, billing, or wire behavior depends on `prefix_sha`.
- Unknown fields, `tools`, `thinking`, `extra_body`, `previous_response_id`:
  preserved by the same map round-trip as today (SwapModel contract).

## Acceptance gates

1. Byte-equality: for a fixture set (unknown fields, nested tools, big ints,
   escapes/unicode, duplicate keys), the hop body built via the doc path is
   byte-equal to the legacy `SwapModel` output. Golden + property assertions.
2. Differential: mutation of the fixture's `model` must change the output
   (a parse-free stub cannot pass).
3. All existing tests green with zero edits to their call shapes.
4. `gofmt`/`vet`/`build`/`go test ./... -count=1`/`-race` green.
5. Benchmarks: `InspectAndRoute` and `UpstreamRoundTrip` at 64 KiB/1 MiB
   measured before/after on the same machine; the report cites both numbers.
   No CI gates (machine-dependent).

## Explicit non-changes

- No new JSON library (stdlib only).
- No compression behavior change; engines keep operating on messages.
- No flag: this is a behavior-identical refactor, not a feature — the
  byte-equality tests ARE the switch-off equivalent (legacy path stays as the
  doc=nil fallback and is itself regression-tested).
- No change to `translate.*`, multipart STT, admin combo-test, or the ledger.

## Measured results (same box as the M1 baseline, both runs same evening)

Request-level (`BenchmarkEndToEnd`, real handler, warm keep-alive):

| scenario | M1 before | after | delta |
|---|---|---|---|
| non-stream 64 KiB seq | 1 787.8 µs | 1 724.3 µs | −3.5% |
| non-stream 64 KiB par4 | 577.7 µs | 512.9 µs | −11.2% |
| non-stream 64 KiB par16 | 531.1 µs | 479.2 µs | −9.8% |
| stream 64 KiB seq | 2 887.3 µs | 2 409.2 µs | −16.6% |
| stream 64 KiB par4 | 865.5 µs | 840.2 µs | −2.9% |
| stream 64 KiB par16 | 911.2 µs | 786.5 µs | −13.7% |
| non-stream 1 MiB seq | 19 356.3 µs | 14 515.0 µs | −25.0% |
| non-stream 1 MiB par4 | 4 436.6 µs | 3 994.7 µs | −10.0% |
| non-stream 1 MiB par16 | 4 350.4 µs | 3 630.9 µs | −16.5% |
| stream 1 MiB seq | 14 279.9 µs | 10 536.1 µs | −26.1% |
| stream 1 MiB par4 | 4 353.9 µs | 3 721.3 µs | −14.5% |

e2e allocs at 1 MiB: ~748/op → ~690–714/op.

**The honest negative:** `BenchmarkInspectAndRoute` (phase 2 in isolation)
GOT HEAVIER: 207→232 µs @1 KiB, 303→451 µs @64 KiB, 1 695→4 199 µs @1 MiB,
143→~180 allocs. The old `extractModel` was a struct decode that skipped
building maps for the messages array; the one shared parse IS a full map
decode, so the isolated inspect step now pays that cost — and the phase bench
shows exactly one call, hiding that the same map replaces 3–5 decodes per
request. The request-level metric is the one that decides, and it moved
(−25% @1 MiB seq, exceeding the ±10% noise band from the baseline's own
cross-run check). Do not quote the phase-2 regression without this sentence.

Gates (all rc=0): `gofmt -l internal cmd` empty · `go vet ./...` ·
`go build ./...` · `go test ./... -count=1` · `-race` proxy/router/jsonbody ·
`-race` internal/server (tracked run) — plus bites: UseNumber removal makes
`TestWithModel` mangle `12345678901234567890` (red, restored md5-verified);
dropping the model assignment makes the dispatch test watch the upstream
receive `"model":null` (red, restored md5-verified). Family: byte-equality
across doc/legacy paths on 7 fixture classes; stream-flag parity;
captureShape parity (number-literal divergence documented-by-test).

## Locked decisions

1. Shared decode is `map[string]any` + `UseNumber`, mirroring the existing
   SwapModel decode exactly — no partial JSON, no new library, no streaming
   parse.
2. doc is an optional hint: every entry point keeps its byte signature and
   falls back to the legacy per-site decode when doc is nil. No flag: the
   legacy path IS the rollback, and the byte-equality tests keep it honest.
3. Compressed bodies re-open the doc on the rewrite path only (rare; the
   ratio floor declines on hot prefixes), so shape capture and hops always
   describe the bytes actually sent.
4. `prefix_sha` intentionally becomes number-literal-sensitive (json.Number),
   collapsing less than float64 did. Correct for a cache-break detector;
   documented per hash-sensitive tooling.
5. Translation hops, multipart STT and admin combo-test stay on the byte
   path — translation rebuilds the body wholesale; there is nothing to share.
6. The hop swap splices instead of re-marshaling (2026-10-19). Re-marshaling a
   1 MiB body cost ~2.6 ms and ~1 MiB of garbage **per hop**; the splice copies
   the client's bytes and replaces only the top-level `model` value (inserted
   first when absent). Every other byte reaches the upstream as the client sent
   it — key order, whitespace, escapes — where it used to arrive key-sorted
   and re-escaped; `FuzzSpliceMatchesMarshal` pins that both forms decode to
   the identical object. The splice declines, and the map is re-marshaled as
   before, on invalid UTF-8 (the marshal sanitizes it) and on any duplicated
   top-level key: routing reads `model`/`stream` last-wins, and an upstream
   that reads the first would disagree. Nested duplicates carry no routing
   fact and pass through as sent.

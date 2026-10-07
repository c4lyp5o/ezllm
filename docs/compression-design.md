# ezllm compression design (M5)

Source of truth: rev4 plan §4 (`~/.hermes/plans/2026-10-05_223000-ezllm-v1-rev4-namespace-compression.md`)
+ `compression_profiles` schema. This doc restates the contract as shipped code and
records every design decision where the plan left latitude.

## Scope (M5)

| Piece | M5 | Later |
|---|---|---|
| `session_dedup` engine | ✅ | |
| `rtk` engine (`scope: tool_only`) | ✅ | |
| Profile store + admin CRUD | ✅ | |
| Resolution: request header → combo → off | ✅ | client default, global default (M6.5) |
| Ledger metrics (`prompt_tokens_pre`, `tokens_saved`, `compression_ms`, `compression_applied`) | ✅ | |
| Stats: compression/savings granularities | ✅ (already summed by `UsageReport`) | |
| `headroom` + `lite` engines | ✅ (M6.5 phase 2, shipped) | |
| `caveman` (prose condensation) | | after its safety design pass (pro) |
| Profile editor page (drag-order stages) | | M6.5 (Compression page stays a read-only preview) |
| Anthropic `/v1/messages` rtk scoping | | M6.5 (shape passes through unchanged today) |

## The §4.2 safety contract (non-negotiable, enforced in `internal/compress`)

1. **Never modify the final message** — `exempt_last_turn` (default on): engines receive
   `messages[:len-1]` only; the final message is appended back byte-identical.
2. **Never modify tool-call arguments or structured tool results** — `tool_calls` on
   assistant messages are never read or written; `role:"tool"` messages are never dropped
   or structurally changed (pairing stays intact); `rtk` may filter *text lines* inside
   tool results only, never mid-JSON (content that parses as JSON is skipped whole).
   `headroom` is the single sanctioned exception: it may rewrite a *whole* pure JSON-array
   payload into the lossless columnar form — never a partial edit, never a drop, pairing
   intact, every value round-tripping (`json.Number`).
3. **Structural integrity post-compression** — fenced regions (` ``` `) are atomic units:
   rtk never splits one. An unclosed fence anywhere ⇒ that content is skipped entirely.
   Any engine failure or integrity miss ⇒ the ORIGINAL body bytes are sent, unchanged.
4. **`min_compress_ratio` floor** (default 0.05) — if `saved/pre < floor`, the original
   body is sent; `compression_applied=0` with `prompt_tokens_pre` still recorded.
5. **`fail_open` always** — any error/timeout ⇒ original request proceeds. Compression is
   an optimization, never a failure mode.
6. **Ledger records the truth** — profile resolved ⇒ `compression_profile` name,
   `prompt_tokens_pre`, `tokens_saved`, `compression_ms`, `compression_applied` (0 on any
   skip). No profile resolved ⇒ all compression columns stay zero/NULL.

Additional gate: `auto_trigger_tokens` (default 0 = always) — above N, engines only run
when the pre-compression estimate reaches N.

## Resolution chain (M5 slice)

`x-ezllm-compression: <profile-name|off>` request header (also the proof hook)
→ combo's `compression_profile_id` (bare-name/`requested` shape only, i.e. combos)
→ off.

Unknown header name or disabled profile ⇒ skip (fail-open), `compression_applied=0`.

## Engine decisions (where the plan left latitude)

### `session_dedup` — first stage in every pipeline

Content-hash duplicate removal, deterministic, `sha256(role \x00 content)`:

- **Droppable**: `system`, `user`, `assistant` messages whose content is a string OR an
  array of `{"type":"text"}` blocks only, and which carry **no `tool_calls`**.
- **Never dropped**: `role:"tool"` (would break call/result pairing — `rtk` covers these),
  any message with `tool_calls`, any message whose content array contains a non-text block
  (`tool_result`, images, …), and the final message (rule 1).
- Later byte-identical duplicates are dropped; the first occurrence stays in place.
  Byte-identical is the whole safety argument: no wording, order, or meaning is invented —
  we only delete a repeat the model has already seen at an earlier position.

### `rtk` — tool-output filtering, `scope: tool_only`

Applies to `role:"tool"` messages with **string** content only. Line-unit algorithm:

1. Skip if content parses as JSON (never mid-JSON, rule 2).
2. Split into units: plain lines + fenced regions as atomic units; unclosed fence ⇒ skip.
3. Keep: first `keep_lines` units (head context, default 6), last `keep_lines` units,
   and every unit matching an error signature (`error`, `failed`, `fatal`, `panic`,
   `exception`, `traceback`, `exit code <non-zero>`, …).
4. Drop the rest of the middle; each contiguous dropped run becomes one marker line
   `[ezllm/rtk: N lines elided]`.
5. If the result is not strictly smaller, send the original.

This is what takes `tsc` 7771 → 20 style output down without touching the error line.
Non-noise middle lines drop too — that is the point (line 2000 of a build log is noise);
the error-class always wins, so a wall-of-errors output simply fails the ratio floor and
passes through untouched.

### `headroom` — lossless columnar JSON, `scope: tool_only`

SmartCrusher-style tabular compaction (omniroute reference): a homogeneous JSON-array
payload pays each key **once** instead of once per row. Applies to `role:"tool"` messages
whose trimmed content is a pure JSON array of objects sharing **one key set**.

Wire form — marker line + columnar JSON, together the whole content:

```
[ezllm/headroom: 10 rows columnar, lossless — row i = i-th element of each column]
{"id":[1,2,3],"name":["inventory-row-001",...]}
```

- **Lossless by construction**: zip the columns back to recover each row. Values are
  exact — the encoder round-trips numbers through `json.Number`, so 64-bit ids never take
  a float64 detour. Key order may differ from the input (JSON objects are unordered);
  nothing else changes.
- **Skips (original bytes sent)**: not a JSON array, rows that aren't objects, ragged key
  sets, fewer than `min_rows` rows (option, default 4), or a result not strictly smaller
  than the input — the ~85-byte teaching marker is real cost, so small payloads are
  correctly refused rather than "compressed" into something bigger.
- Options: `min_rows` (default 4).

### `lite` — whitespace-only cleanup

Omniroute's safe Lite tier: zero semantic change, always-on. It never rewrites words —
filler stripping and phrase condensation belong to `caveman`, which stays gated on its
safety design. To string content of any role it:

1. strips trailing spaces/tabs on lines **outside** fences,
2. collapses blank-line runs down to `blank_lines` (option, default 1),
3. trims blank lines at the content's edges.

Fence interiors are byte-identical (same atomic rule as `rtk`; an unclosed fence skips the
content whole) and pure-JSON content is skipped, so `headroom` owns payloads. Not strictly
smaller ⇒ original kept.

- Options: `blank_lines` (default 1).

### Token estimates

No tokenizer dependency (pure-Go / no-CGO constraint): `estimate = ceil(runes/4) + 1`
applied to the marshaled `messages` array before and after. Savings ratios are therefore
**request-derived measurements of the actual bytes we rewrote**, not marketing numbers;
`calls.tokens_in` remains the provider-truth counter. The floor and the ledger use the
same estimator, so ratio decisions and reported savings are internally consistent.

## Where it runs

Server-side in `inference()` — once per request, after candidate resolution, **before**
`ForwardCandidates` (hops only swap `model`, so every candidate receives the same
compressed body). The original body bytes are reused verbatim on every skip path
(fail-open means *byte-identical passthrough*, not re-marshal).

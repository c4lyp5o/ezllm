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
| `caveman` (prose condensation) | ✅ (M7, shipped) | deterministic rule packs, text-only, protected spans + span-integrity gate, `lite`/`standard`; aggressive+ultra = stage composition |
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
filler stripping and phrase condensation belong to `caveman` (M7, shipped — see its own
section below). To string content of any role it:

1. strips trailing spaces/tabs on lines **outside** fences,
2. collapses blank-line runs down to `blank_lines` (option, default 1),
3. trims blank lines at the content's edges.

Fence interiors are byte-identical (same atomic rule as `rtk`; an unclosed fence skips the
content whole) and pure-JSON content is skipped, so `headroom` owns payloads. Not strictly
smaller ⇒ original kept.

- Options: `blank_lines` (default 1).

### `caveman` — prose condensation, `scope: text_only`

The only **lossy** engine in the set, and the reason it waited for a safety design pass.
Lossy here means *words can disappear*, so the whole design is about proving nothing
semantic disappears.

**Mechanism — deterministic rule packs, never a model.** Caveman applies curated regex
substitutions (`verbose request → directive`) to **prose only**. No LLM, no tokenizer, no
embedding: the same input always yields the same output, byte-for-byte. That is what makes
it testable and auditable. An LLM rewriter would fail contract rule 5 (fail-open must be
*byte-identical*, and an LLM cannot promise determinism).

**Intensity — one knob for prose aggressiveness.**

| Intensity | Packs that fire | Risk | When |
| --- | --- | --- | --- |
| `lite` | filler/cleanup class only | near-zero (whitespace-adjacent) | always-on default |
| `standard` | + context + dedup packs | low, but *words change* | long sessions, opt-in |

omniroute's `aggressive` and `ultra` are **not strengths of caveman** — they are caveman
plus *other engines* (history/tool summarizers, pruning). ezllm already ships those as
first-class stages, so the equivalent is **composition, not a flag**:

```jsonc
{"stages":[{"engine":"session_dedup"},{"engine":"rtk","options":{"keep_lines":6}},
           {"engine":"headroom"},{"engine":"caveman","options":{"intensity":"standard"}}]}
```

One knob governs prose; the pipeline governs structure. Inventing a `caveman:ultra` that
secretly also ran dedup/rtk would make the ledger lie about what compressed the prompt.

**Protection layers — prose is the *only* editable surface.**

1. `exempt_last_turn` (contract rule 1) — the live question is never rewritten. Caveman
   condensing the user's actual ask is the worst possible failure mode, so the newest
   message is out of scope by construction.
2. `scope: text_only` — **only** `content` strings on plain text messages. Never
   `role:"tool"` (headroom owns payloads), never assistant `tool_calls`, never the `model`
   field or any non-message key.
3. Fenced regions are **atomic and untouched** (contract rule 3) — `splitUnits` groups each
   ``` block as one unit and caveman skips it whole. Code, diffs, JSON samples and command
   output survive verbatim even inside a prose message.
4. **Inline protection spans** — the text is segmented before any rule runs, and protected
   spans are re-inserted verbatim afterwards:
   - inline `code`, fenced blocks (above)
   - URLs, absolute/relative **file paths**, `identifiers_like_this`, camelCase/`snake_case`
   - numbers with units, quoted strings, email addresses, IP:port, version strings
   - anything matching a rule pack's `preserve` patterns
5. **Whole-content skip** — content that parses as JSON is skipped (the same guard rtk and
   lite use); an unclosed fence anywhere ⇒ that content is skipped entirely.

**Rule packs — data, validated at load.**

Pack shape mirrors the reference format so packs stay portable:
`{language, category, rules:[{name, pattern, replacement, replacementMap?, flags?,
context?, category?, minIntensity?, description?}]}`.

- **Load-time validation** (`caveman_rules.go`): every pattern must compile, every rule
  needs `name` + `pattern`, `minIntensity` must be a known level. A bad rule **disables its
  pack with a diagnostic** — it never half-applies. (Reference behaviour: untrusted/custom
  packs are skipped with diagnostics rather than failing the request.)
- **Built-in packs are embedded** in the binary (`go:embed`) so a deployment cannot be
  silently weakened by a missing file; external packs are opt-in and untrusted by default.
- **Rule hygiene gate** — a rule whose replacement *adds* length is rejected at load.
  Caveman must only ever shrink.

**Savings gates (the "can only ever help" promise, enforced per-content).**

- Skip content where caveman yields **no strict byte reduction**.
- **Emptied-segment guard**: a prose segment that carried words cannot be reduced to
  nothing. Whitespace-only segments may vanish (that is just cleanup).

  > A message-level **word-loss ceiling** (`max_word_loss_pct`) was implemented and then
  > REMOVED, and the reason matters: it cannot tell "deleted five filler words" from "ate a
  > paragraph". On a genuinely verbose prompt it false-rejected the exact messages worth
  > compressing — measured 62% word loss on a message that was 5/8ths padding, with a
  > *correct* rewrite. Segment granularity plus span integrity is the honest signal; a
  > percentage-of-words threshold is not.
- Skip when a **protected-span integrity check** fails: every protected span present in the
  input must still be present in the output, in order. Any missing span ⇒ keep the original.
  This is the check that makes a *lossy* engine safe enough to run unattended.
- Then the profile-level `min_compress_ratio` floor applies as usual (contract rule 4).

**Observability — you must be able to see what it did.**

- Ledger gains `calls.compression_rules_fired` (schema v3) — the count of prose rules that
  actually rewrote something, so a surprising saving is attributable instead of mysterious.
  Always 0 for the other four engines. `tokens_saved` records the size.
- The dashboard Compression page shows intensity per profile and the fired-rule count for
  recent calls — "what changed my prompt" is one click away, never a mystery.

**Language packs.** English built-ins only at first. Pack loading is per-language, so
`id`/`ms` packs can be added later as data without touching engine code. Non-English prose
simply matches fewer rules and degrades to *less* compression — never to damage.

**Explicitly out of scope for v1** (each needs its own design + pro review): history
summarization, LLM-assisted condensation, tool-result pruning beyond rtk, and any pack that
rewrites inside fenced regions.

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

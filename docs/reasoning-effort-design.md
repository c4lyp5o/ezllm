# Reasoning effort: one vocabulary per model, learned from the refusal

Status: shipped on `melissa/fix-review-and-ledger` (schema v29). Not yet released.

## The defect

`reasoning_effort` was forwarded from the client to every hop unchanged. Three
providers in the ledger rejected the same request for three different reasons,
and each rejection cost a failed request at the edge:

| Account | Model | What the provider said |
| --- | --- | --- |
| opencode-go | `muse-spark-1.3-contributor` | `reasoning_effort 'none' is not supported … Supported values: [minimal, low, medium, high, xhigh, max]` |
| command-code | `meta/muse-spark-1.3-contributor`, `xiaomi/mimo-v2.6-flash`, `minimax/minimax-m3.1-flash:free` | `Invalid option: expected one of "off"\|"low"\|"medium"\|"high"\|"xhigh"\|"max"` |
| openrouter / opencode-go | `minimax/minimax-m3.1-flash` | `Reasoning is mandatory for this endpoint and cannot be disabled` / `requires adaptive thinking; reasoning effort none is not allowed` |

Two conclusions, both measured:

1. It is **not** a boolean capability. The field is accepted; one *value* is
   refused. A `supports_reasoning` flag would fix none of these and would break
   minimax in the opposite direction (there, reasoning must never be removed).
2. Every provider that refused **named the alternatives in the refusal**. The
   gateway does not have to guess a vocabulary — it has to read one.

## Locked decisions

- **The rule is per `(account, model)`.** Stored on the `models` row as
  `reasoning_vocab` (CSV, provider order preserved) and `reasoning_required`
  (1/0/NULL). NULL means "nothing known".
- **NULL is inert.** An unknown rule returns the client's body byte-identical.
  This is what makes the feature safe to ship to a fleet where most models have
  never been probed.
- **A clamp never raises effort.** The value chosen is the highest allowed at or
  below what was asked; if nothing is that low, the cheapest allowed. Buying
  more reasoning than the client asked for is a cost regression, not a fix.
- **`required` + a disabled effort (`none`/`off`) with no vocabulary = drop the
  field**, letting the provider's own default apply. Inventing a value the
  provider did not name is the one thing worse than the 400.
- **An allowed value is left alone.** No rewrite, no re-encode: the common path
  sends exactly the bytes the client sent.
- **Learning is two-sided, writing only facts that carry information.** The key
  test records what a probe refusal taught it; live traffic reports it on
  `Result.LearnedReasoning` and the server persists it. An empty rule is a
  no-op, so a mis-parse can never erase a known vocabulary.
- **Scope is the top-level `reasoning_effort` string.** Anthropic's `thinking`
  block and a nested `reasoning.effort` (Responses) are different vocabularies;
  they are left untouched rather than approximated.
- **A clamp is never silent.** It rewrites what the client asked for, so the
  console records both values (`asked=none sent=minimal`) at Info, once per
  changed hop, and a mandatory-reasoning drop is reported as a drop. An effort
  the model already accepts logs nothing — the common path stays quiet. A
  learned vocabulary is Info; a write that matched no catalog row (a combo hop,
  a retired model) is Debug, because it is routine and otherwise unexplainable.
  See `docs/logging.md`.

## Where it lives

- `internal/provider/reasoning.go` — `ParseReasoningRule` (reads the refusal)
  and `ReasoningRule.Clamp` (maps a value onto it). Pure, no I/O.
- `store.ModelFacts.Reasoning` — travels with the pin and capability reads, so
  the router needs no extra query.
- `router` — `selectWire` attaches the rule to `proxy.Route` from the same
  memoized facts row; a second `factsFor` per hop doubled the row reads.
- `proxy.prepareHopBody` — clamps **before** the wire branches, so the native
  swap and every translation inherit it. A clamp invalidates the shared
  `jsonbody.BodyDoc` (it describes bytes no longer sent), so the native path
  re-parses instead of splicing into the original. It returns the
  `*effortChange` it applied, so the dispatcher can report it without the clamp
  helper doing I/O: the clamp stays pure and the log call sits in
  `ForwardCandidatesDoc`, after the error branch (a hop skipped for an
  untranslatable field never dialled, so it changed nothing on the wire).
- `server.finishDispatch` — persists `Result.LearnedReasoning`, detached from
  the request context so a client that hung up still gets the lesson kept.

## Traps this re-taught

- The catalog refresh (`UpsertModels`) replaces a model row wholesale and
  restores only what its snapshot carries. The new columns are in that snapshot;
  without it, every boot would forget every vocabulary.
- A migration test that opens a database *created by this binary* cannot prove
  the `ALTER` path: the columns already exist. Drop them first, then stamp the
  previous version.
- A live-learning hook placed only in the failover branch teaches nothing for a
  sole hop that answers 400 — which is the common case. It sits before the
  failover/commit split.
- `commit` builds its **own** `Result`; anything learned must be copied onto it
  or it dies with the request.

## Not done (deliberate)

The teaching request itself still fails. The rule is learned and the next
request is clamped, but there is no in-request retry with the clamped body. That
is one extra upstream call on a path that just failed; it can be added later
without changing the data model.

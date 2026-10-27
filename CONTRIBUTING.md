# Contributing to ezllm

Thanks for working on ezllm. This file holds the workflow, the conventions,
and — importantly — the **known issues and standing gotchas** that bite
people who don't know them yet.

## Workflow

```bash
make test-short   # local functional gate, before every commit
make test-quiet   # test-short pinned to two cores at low priority, one package at a time
```

Run Go tests with `-short` on this machine, including focused package tests.
Do not run `make check`, `make test`, `make test-race`, or `make perf` locally.
CI separately runs the full suite. Run `go vet ./...` and `make build` as
appropriate for code changes.

- **gofmt is a hard gate.** CI rejects the build if `gofmt -l cmd internal`
  prints anything. `make fmt` fixes it.
- **Avoid cached test results.** `make test-short` uses `-count=1`; CI also
  sets `-timeout 15m`. Do not
  "verify" from cached results — cached passes have masked real changes here
  before.
- **Web changes:** run `npm run build` inside `web/`. The `web/dist/` output
  is committed on purpose — `cmd/ezllm` embeds it via `go:embed`, so a UI
  change that doesn't rebuild `dist` simply never ships. CI type-checks and
  builds and compares the committed bundle with fresh output, normalizing Vite
  hash suffixes — rebuild locally before committing.
- **Version bumps:** `internal/server/version.go` and the `Current:` line in
  `VERSION.md` move **together** in one commit. `version_test.go` fails
  otherwise, and `GET /healthz` serves whatever the constant says.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/`, `internal/` | Go source, with `_test.go` **in-package** (see below) |
| `tests/` | Live proof scripts (Python) — run against a real instance, not part of `go test` |
| `web/tests/` | Playwright screenshot (`shot-*.mjs`) and check (`verify-*.mjs`) drivers |
| `scripts/` | Operator utilities: `backup_sqlite.py` (online SQLite backup), `ezllm-mac.sh` (macOS binary installer + launchd agent) |
| `docs/` | Design records and runbooks — decisions, not status |
| `EXPLORING.md` | Codebase tour: reading order, invariants, landmarks |
| `CHANGELOG.md` | Milestone-tagged change narrative |
| `VERSION.md` | Release history — semantic versioning |

### Why Go test files are not in `tests/`

Go requires `_test.go` files to live in the **same package directory** as the
code they test (in-package tests reach unexported identifiers; the toolchain
will not pick up test files from a separate tree). Moving them would break
`go test -short ./...`. The `tests/` folder therefore holds only the *external*
proof scripts, which are not part of the Go build. This is a toolchain
constraint, not a preference.

## Testing philosophy

- Unit/table tests live next to the code, bite-proven: every fix should come
  with a test that **fails without the fix**, and existing tests are never
  weakened to match buggy behaviour.
- `tests/prove_*.py` are **live proofs** — they exercise a running instance
  through the real admin API (never against production; use a scratch rig).
- `web/tests/verify-*.mjs` are **UI checks**. The regression drivers built on
  `web/tests/mockdash.mjs` serve the built `web/dist` with a mocked API, so they
  need only `npm run build` (`node tests/verify-<name>.mjs` from `web/`); each
  exits non-zero on a failed check. `shot-*.mjs` capture review screenshots.
- `make verify` runs `cmd/m2verify` — acceptance invariants against a live
  ledger, run after any soak.

## Memory: the test suite and 4 GiB cgroups

Known, measured (2026-10-17):

- The full suite **peaks at ~4.21 GiB RSS** — dominated by
  `TestLatencyPercentiles` (thousands of real loopback requests) in
  `internal/server`.
- `GOMEMLIMIT=3GiB / GOGC=50` (set by `make test`, same as CI) is a *soft
  heap* limit. It does **not** cap RSS; transient traffic still peaks at
  ~4.21 GiB.
- In a **4 GiB memory cgroup** (e.g. a Hermes worker scope), that peak is an
  OOM kill: `server.test` dies at ~3.98 GiB anon-rss. The machine's free RAM
  is irrelevant — the cgroup ceiling is what kills it.
- **Use `make test-short` locally.** It skips the percentile/retained-memory
  harnesses (`testing.Short()`), runs the functional suite, and peaks at
  ~0.18 GiB. CI runs the full suite separately.

## Known issues / open threads

Lingering items — fix or close them deliberately, don't rediscover them:

Reviewed for 1.0 (2026-10-22): none blocks the release. They concern proof,
test tooling and docs upkeep, not a running install, and stay open as
post-1.0 work.

1. **Cloudflare Workers AI has no live end-to-end proof.** The key-test path
   is unit-covered and the endpoint composition is implemented, but the
   account-scoped live proof was still pending an account ID (open as of
   2026-10-13).
2. **No `goleak` in the suite.** Goroutine-leak detection was deliberately
   out of scope for the perf work; adding it will surface real findings and
   some noise in the proxy/server packages — budget a pass for it.
3. **No CI latency gate.** Percentile numbers exist in `docs/perf-baseline-m1.md`
   but nothing enforces them — a stable reference environment is needed
   before a regression gate means anything.
4. **Older Playwright drivers need a live instance.** The `shot-*.mjs`
   drivers and `verify-feed*`, `verify-ranges`, `verify-stats` expect a
   dashboard on port 20199, an `EZLLM_TOKEN_HERMES` token and fixed
   screenshot paths. Move them onto `mockdash.mjs` before relying on them
   elsewhere.
5. **`docs/API.md` JSON examples are hand-curated.** Refresh them from live
   responses after contract changes (curl the endpoint, reshape
   field-for-field) rather than editing from memory.

## Documentation discipline

Docs are claims about code — verify before writing:

- Cite code by file and symbol (`proxy.go` `Forward`), not `file.go:NN`:
  line numbers drift with every edit. Check the symbol exists before citing it.
- No stale markers: `⏳ TBD TODO FIXME` in `*.md` should read zero.
- Status lines age: prefer "shipped — <what>, verified <date>" over bare
  milestone tags.
- Milestone history goes in `CHANGELOG.md` (reverse-chronological, dated);
  dated version bumps go in `VERSION.md`; design decisions go in `docs/`.
  README stays the front door: what it is, how to run it, where things live.

## Releasing

Both publishers are gated by `test.yml`: a red suite never produces an image or
an asset.

1. **Bump the version in one commit.** `internal/server/version.go` and the
   `Current:` line of `VERSION.md` move together; `version_test.go` enforces the
   pairing and `/healthz` serves the constant.
2. **Tag, then push the tag** — `git tag vX.Y.Z && git push --tags`. Each
   publisher runs a `version` job first and **fails** if the tag does not equal
   the constant in `version.go` (a prerelease suffix such as `-rc1` is allowed).
3. **`release.yml`** builds the five standalone binaries — linux amd64/arm64,
   darwin amd64/arm64, windows amd64 — packs each with `config.example.yaml` and
   `README.md`, writes `SHA256SUMS`, and attaches them to the GitHub release
   with generated notes. Everything is `CGO_ENABLED=0`, so the cross-compile
   needs no toolchain; nothing is signed (no Authenticode, no notarization).
4. **`docker-publish.yml`** builds the multi-arch image (amd64 + arm64,
   cross-compiled in the builder stage — never emulated) and pushes it to GHCR.
   **`:latest` is master-only:** a tag build publishes just its version tag, so
   tagging an old commit cannot roll `latest` backwards.
5. **Verify what shipped, from outside.** The release lists five assets plus
   `SHA256SUMS`, and the running deployment reports the new version at
   `/healthz`. A schema bump additionally needs the rehearsal in
   `docs/install-binary.md` §7 (boot the new binary against a COPY of the data
   directory, compare counts and `schema_version`).

## Commit conventions

- Conventional-commit style, lowercase: `fix:` (default), `feat:`, `docs:`,
  `chore:`, `refactor:`, `test:`, `perf:`, `style:`, `build:`.
- Small, focused commits. Subject on one line; the body carries the *why* — a
  measured number, a trap the diff cannot show, what was deliberately not done.
  (Recent history is the reference: most commits have bodies.) Narration of the
  diff does not earn one.
- Never commit secrets: `config.yaml`, `data/`, and token files are
  gitignored for a reason.

# Contributing to ezllm

Thanks for working on ezllm. This file holds the workflow, the conventions,
and — importantly — the **known issues and standing gotchas** that bite
people who don't know them yet.

## Workflow

```bash
make check        # gofmt + go vet + full test suite + build — run before every commit
make test-short   # functional suite only (see "Memory" below)
```

- **gofmt is a hard gate.** CI rejects the build if `gofmt -l cmd internal`
  prints anything. `make fmt` fixes it.
- **`go test` runs with `-count=1 -timeout 15m`** (mirrors CI). Do not
  "verify" from cached results — cached passes have masked real changes here
  before.
- **Web changes:** run `npm run build` inside `web/`. The `web/dist/` output
  is committed on purpose — `cmd/ezllm` embeds it via `go:embed`, so a UI
  change that doesn't rebuild `dist` simply never ships. CI type-checks and
  builds, but cannot verify the committed dist matches the sources byte-for-byte
  (Vite hashes depend on the checkout path) — rebuild locally before committing.
- **Version bumps:** `internal/server/version.go` and the `Current:` line in
  `VERSION.md` move **together** in one commit. `version_test.go` fails
  otherwise, and `GET /healthz` serves whatever the constant says.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/`, `internal/` | Go source, with `_test.go` **in-package** (see below) |
| `tests/` | Live proof scripts (Python) — run against a real instance, not part of `go test` |
| `web/tests/` | Playwright screenshot (`shot-*.mjs`) and check (`verify-*.mjs`) drivers |
| `docs/` | Design records and runbooks — decisions, not status |
| `EXPLORING.md` | Codebase tour: reading order, invariants, landmarks |
| `CHANGELOG.md` | Milestone-tagged change narrative |
| `VERSION.md` | Release history — one version per change date |

### Why Go test files are not in `tests/`

Go requires `_test.go` files to live in the **same package directory** as the
code they test (in-package tests reach unexported identifiers; the toolchain
will not pick up test files from a separate tree). Moving them would break
`go test ./...`. The `tests/` folder therefore holds only the *external*
proof scripts, which are not part of the Go build. This is a toolchain
constraint, not a preference.

## Testing philosophy

- Unit/table tests live next to the code, bite-proven: every fix should come
  with a test that **fails without the fix**, and existing tests are never
  weakened to match buggy behaviour.
- `tests/prove_*.py` are **live proofs** — they exercise a running instance
  through the real admin API (never against production; use a scratch rig).
- `web/tests/verify-*.mjs` are **UI checks** (screenshot + assertion) against
  a local dashboard instance; `shot-*.mjs` capture review screenshots.
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
- **Use `make test-short` there.** It skips the percentile/retained-memory
  harnesses (`testing.Short()`), runs every functional test, and peaks at
  ~0.18 GiB. Full-suite runs belong on uncapped shells or in CI.

## Known issues / open threads

Lingering items — fix or close them deliberately, don't rediscover them:

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
4. **Playwright drivers hardcode absolute paths.** `web/tests/*.mjs` read a
   fixed secrets file, a fixed screenshot dir, and a fixed port (20199).
   They work on one machine; parameterize via env before relying on them
   elsewhere.
5. **`docs/API.md` JSON examples are hand-curated.** Refresh them from live
   responses after contract changes (curl the endpoint, reshape
   field-for-field) rather than editing from memory.

## Documentation discipline

Docs are claims about code — verify before writing:

- Landmarks (`file.go:NN`) get checked against the actual file, not trusted
  from the previous revision.
- No stale markers: `⏳ TBD TODO FIXME` in `*.md` should read zero.
- Status lines age: prefer "shipped — <what>, verified <date>" over bare
  milestone tags.
- Milestone history goes in `CHANGELOG.md` (reverse-chronological, dated);
  dated version bumps go in `VERSION.md`; design decisions go in `docs/`.
  README stays the front door: what it is, how to run it, where things live.

## Commit conventions

- Conventional-commit style, lowercase: `fix:` (default), `feat:`, `docs:`,
  `chore:`, `refactor:`, `test:`, `perf:`, `style:`, `build:`.
- Small, focused commits; single-line subjects, no bodies.
- Never commit secrets: `config.yaml`, `data/`, and token files are
  gitignored for a reason.

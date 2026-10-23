# Spec: codex-bridge — ChatGPT subscription via the official Codex CLI

**Status:** M0 measured 2026-10-23; stages 1–3 built on `feat/codex-bridge` (design §2.13–2.14); push + CI + deploy next (Calypso gates)
**Scope:** Codex only. Gemini CLI is deferred: it needs Node, which conflicts with
locked decision #3 in `docs/claude-subscription-design.md`. `geminiweb` already covers that path.
**Pattern:** the claude-bridge (`internal/claudebridge`, `docs/claude-subscription-design.md`).
That doc is binding for anything this spec doesn't override.

## Objective

Serve **ChatGPT Plus/Pro subscription capacity** to ezllm clients through the
**official Codex CLI**, the same way claude-bridge serves the Claude subscription.
Clients keep pointing at ezllm. The ezllm side is an ordinary `openai-compatible`
account that dials a loopback surface inside the ezllm process.

**Hard constraint ("no trouble").** Only the official `codex` binary talks to the
subscription. ezllm's Go process never reads, refreshes or replays the ChatGPT
OAuth tokens in `auth.json`, and never impersonates the Codex client. Spawning the CLI is the only way in.

**Second hard constraint: nothing executes.** Codex is a coding agent with shell and
`apply_patch` tools. The bridge must make sure it cannot run a command or write a
file on this box under any input. This has to be *proven* with a sentinel (M2 had the
`write_file` sentinel), not just assumed from flags.

**Users.** The operator (Calypso) adds the account and combo. Consumers are Hermes,
Claude Code and other agents routed through ezllm. Per operator decision, the account
**may sit in fallback chains**, protected by M5 Rules caps (a capped hop is skipped
in a combo, `internal/router/router.go:753`).

### Build plan (operator-approved 2026-10-23; supersedes the original M1/M2)

M0 is done: `docs/codex-subscription-design.md` §2 (measurements) and §4 (decisions).
The target is **full function**; stages are build order, each shippable and tested.

**Stage 1 — transport (text, images, effort).**
1. `internal/codexbridge/rpc.go` — JSON-RPC-over-JSONL client for `codex app-server --stdio`:
   requests by id, notifications to a handler, server→client requests answered (approvals
   always declined). *Test:* a scripted fake app-server.
2. `request.go` — OpenAI chat request → turn: `system` → `baseInstructions`; prior turns →
   flattened transcript; last user turn + images → `turn/start` input; `reasoning_effort` →
   `effort`; tools → 400 `chat_tools_unsupported` (until stage 2). *Test:* table of requests.
3. `runner.go` — spawn with the lockdown argv (code-mode host **off** in stage 1), allowlisted
   env, private temp `HOME`/CWD, semaphore, process-group kill, ephemeral thread; events →
   sink. *Tests:* argv/env table; leak test with a fake CLI (cancel mid-stream → no process).
4. `server.go` / `sse.go` — `/healthz` (exactly two fields), `/status`, `/v1/models`
   (`model/list`, hidden included), `/v1/usage` (`account/rateLimits/read` → percent shape,
   for the provider card), `/v1/chat/completions` stream + non-stream, usage from
   `tokenUsage.total`. *Tests:* httptest against a fake executor.
5. `errors.go` — `codexErrorInfo` → status (usageLimitExceeded/rateLimitExceeded → 429,
   unauthorized → 401, contextWindowExceeded/badRequest → 400, serverOverloaded → 503,
   binary missing → 503, timeout → 504, other → 502), carrying the CLI's own words.
6. `exec` fallback — app-server missing a required method at boot, or the child dying before
   the first byte → `codex exec --json` (text only, single chunk).
7. `cmd/ezllm/codexbridge.go` — env, boot checks (fatal), loopback listener, token file;
   Dockerfile/compose mounts; runbook "Enable" + "Wire it into ezllm".
8. Live proof `tests/prove_codex_bridge.py` — health, auth, catalog, stream, non-stream,
   usage, history recall via the transcript, tool refusal, strace-free sentinel (no file).

**Stage 2 — client tools.** Landlock re-exec helper; code-mode host on when Landlock is
available (or the operator override); `tools` → `dynamicTools`; `item/tool/call` →
`tool_calls`; **continuation**: the child is parked keyed by `tool_call_id` until the
follow-up request answers it (timeout → stateless replay). No Landlock → 400 per the runbook.

**Stage 3 — the rest.** `response_format: json_schema` → `outputSchema`; reasoning summaries →
`reasoning_content`; `/v1/responses` if a client needs it.

**Also in scope (operator, 2026-10-23):** plan windows on the provider cards for both bridges —
claude-bridge from `rate_limit_event`, codex-bridge from `account/rateLimits/read`, both served on
`{base}/usage` in the percent shape.

## Tech Stack

Go 1.27, stdlib `net/http` and `os/exec`. No new dependencies. Official Codex CLI,
bind-mounted read-only from the host, version pinned at M0. Runtime image stays
`distroless/base-debian12` unless M0 shows the binary needs more (then ask).

## Commands

```
Format:      gofmt -l . (must print nothing)
Vet:         go vet ./...
Unit:        go test -short -count=1 ./internal/codexbridge/ ./cmd/ezllm/
Local gate:  systemd-run --user --scope -p CPUQuota=100% -p MemoryMax=2G make test-quiet
Full gate:   git push origin <branch> && gh workflow run test.yml --ref <branch>   (CI only — never local `make check`, this box is the load balancer)
Build:       make build
Live proof:  python3 tests/prove_codex_bridge.py
Spike login: CODEX_HOME=~/.codex-sub codex login
```

## Project Structure

```
internal/codexbridge/          → the bridge: server.go (surface), runner.go (spawn/supervise),
                                 frames.go (native events → OpenAI), modelmap.go, errors.go
internal/codexbridge/testdata/ → redacted native event captures from M0 (*.jsonl)
cmd/ezllm/codexbridge.go       → env config, boot checks, loopback listener (mirrors claudebridge.go)
Dockerfile                     → EZLLM_CODEX_BIN=/opt/codex/codex, EZLLM_CODEX_HOME=/codex-sub
docs/codex-subscription-design.md → decisions + M0 measured baseline
docs/codex-bridge-runbook.md   → mounts, login, pinning, account/combo/Rules setup
tests/prove_codex_bridge.py    → live proof (not part of go test)
```

Env surface: `EZLLM_CODEX_BRIDGE` (`on`), `EZLLM_CODEX_BIN`, `EZLLM_CODEX_HOME`,
`EZLLM_CODEX_BRIDGE_ADDR` (default `127.0.0.1:8791`), `EZLLM_CODEX_BRIDGE_TOKEN_FILE`.
`EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK` (`on` = client tools without Landlock; default off — see
`docs/codex-bridge-runbook.md`).

## Code Style

Mirror `cmd/ezllm/claudebridge.go` and `internal/claudebridge`: comments explain the
*measured* reason behind each constraint, and boot fails loudly instead of degrading silently.

```go
// flags this bridge depends on, asserted at boot: a CLI release can drop or rename
// one while requests still "work", and silent drift here breaks every call.
var codexRequiredFlags = []string{"--json", "--sandbox", "--skip-git-repo-check" /* final list from M0 */}

func startCodexBridge(ctx context.Context, log *slog.Logger) error {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envCodexBridge)), "on") {
		return nil
	}
	...
	if err := assertLoopback(addr); err != nil {
		return err
	}
```

Wire errors are OpenAI-shaped: `{"error":{"type","message","code"}}`. Tabs, gofmt, in-package `_test.go`.

## Testing Strategy

- **Unit (in-package):** frame decoding against M0 `testdata/*.jsonl`. Request → argv/env
  construction, including the allowlist and no-execution flags. Tool refusal. Error
  classification table. Usage mapping. Loopback assertion.
- **Leak test:** cancel mid-stream → process group gone (mirror `runner_leak_test.go`),
  using a fake CLI script instead of the real binary.
- **Server tests:** `httptest` against an `Executor` fake, covering auth, `/healthz` with
  exactly two fields, `/v1/models`, SSE shape and `[DONE]`.
- **Live proof:** health contract, auth, catalog, stream, non-stream, usage fields,
  history recall, tool refusal, no-execution sentinel, plan-error passthrough where observable.
- Each fix ships with a test that fails without it. No existing assertion is relaxed.

## Boundaries

- **Always:** spawn only the official binary. Isolated `CODEX_HOME`. Private temp CWD per
  request. Allowlisted env. Loopback-only bind. Boot assertions. `make test-quiet` +
  CI before reporting tested. Bump `internal/server/version.go` + `VERSION.md`
  together. `npm run build` only if `web/` is touched (not planned).
- **Ask first:** changing the runtime base image or adding Node. Any schema change.
  Refactoring shared code out of `internal/claudebridge` (this spec copies the few helpers instead).
  Any route that would hand Codex an executable tool. Deploy/push to master (Calypso gates).
- **Never:** read or forward `auth.json` tokens from Go. Bake the CLI into the image.
  Commit `auth.json`, captures with tokens/account IDs, or `config.yaml`. Add fields to
  `/healthz`. Silently drop client tools. Run local `make check`.

## Success Criteria

1. M0: done — `docs/codex-subscription-design.md` §2 answers all 11 spike questions with
   measured numbers; the operator's decisions are in §4.
2. Each stage: every item in the build plan has a unit or live check, the live proof is green,
   and CI `test.yml` is green on the branch.
3. With the account in a combo behind a Rules cap, hitting the cap skips the hop
   instead of erroring the request (verified once live).
4. claude-bridge behaviour is unchanged apart from the new `GET /v1/usage`:
   `go test ./internal/claudebridge/` and `tests/prove_claude_bridge.py` still pass.

## Open Questions

All five were answered by the operator on 2026-10-23 — see
`docs/codex-subscription-design.md` §4 (history: flattened transcript, not the Codex Cloud
field; surface: Chat Completions; caps: Rules only; models: all; install: standalone, pinned copy).

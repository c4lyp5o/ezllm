# GitHub Copilot third-party access — policy evidence (October 2026)

**Question asked (Calypso, 2026-10-15):** can ezllm add GitHub Copilot as an OAuth provider
(device flow) calling an OpenAI-compatible completions endpoint?

**Short answer: NO.** Not for third-party OAuth apps against the raw completions
endpoint. The only officially supported programmatic path today is the **Copilot SDK**
(agent runtime, GA 2026-06-02), which is *not* an OpenAI-compatible completions API.
Evidence with URLs and dates below. All citations from the public web, researched
2026-10-16.

## (1) Does GitHub publicly allow third-party OAuth apps for Copilot model access?

**No.** Confirmed by GitHub staff/community in 2026:

- 2026-08-29, GitHub Community discussion #206143
  (https://github.com/orgs/community/discussions/206143#discussioncomment-18200574):
  "When you authenticate via a custom OAuth App using the copilot scope, you are
  routed to a standard API tier. GitHub restricts this token tier to a specific
  whitelist of stable models… because you are querying api.githubcopilot.com
  directly, which is technically an undocumented endpoint for public developer use."
- 2026-03, GitHub Community discussion #188413
  (https://github.com/orgs/community/discussions/188413): third-party tools request
  "Copilot OAuth whitelisting" via a "Copilot Partner Program" — **no public
  process exists**; dedicated client IDs (e.g. OpenCode, Jan 2026) are
  exceptional, case-by-case.
- The Copilot SDK (GA 2026-06-02,
  https://github.blog/changelog/2026-06-02-copilot-sdk-is-now-generally-available/)
  officially supports GitHub OAuth App auth — but for building **on the SDK agent
  runtime**, not for the raw completions endpoint
  (https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/authenticate:
  "GitHub OAuth App — Apps acting on behalf of users via OAuth — Copilot
  Subscription Required: Yes").

## (2) Official auth path for IDE vs non-IDE clients

- **IDE/first-party clients** (VS Code, JetBrains, Xcode, Eclipse, Neovim, Copilot
  CLI, GitHub Copilot App): dedicated internal client IDs (e.g.
  `Iv1.b507a08c87ecfe98` — VS Code/Copilot CLI), device flow (RFC 8628) via
  `https://github.com/login/device` (Copilot CLI docs:
  https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/authenticate-copilot-cli).
  Token exchange: user OAuth token (`gho_`/`ghu_`) →
  `https://api.github.com/copilot_internal/v2/token` → short-lived Copilot session
  token. Model access is gated by client ID.
- **Non-IDE clients**: the only *documented* path is the Copilot SDK with a user's
  OAuth token (https://docs.github.com/en/copilot/how-tos/copilot-sdk/setup/github-oauth).
  Calling `api.githubcopilot.com` directly works for some tools but is
  undocumented, unsupported, and restricted (see §3).

## (3) Reports of rejection / endpoint changes in 2026

| Date | Report | Substance |
|------|--------|-----------|
| 2026-02-20 | Community #187602 (https://github.com/orgs/community/discussions/187602) | `api.githubcopilot.com/models` silently began returning only ~8 legacy OpenAI models (gpt-4o family) for third-party tokens; GPT-5.x/Claude/Gemini disappeared from API but still work in IDEs. Server-side allowlist/routing change keyed to `Copilot-Integration-Id` + token type. |
| 2026-08-29 | Community #206143 (above) | Third-party OAuth = "standard API tier" with fixed stable-model whitelist; endpoint called "technically an undocumented endpoint for public developer use". |
| 2024-2025 | ezllm's own parked experiment (2026-10-09 wait — actually parked 2025-10-09, see mnemosyne) | `copilot_internal/v2/token` exchange answered HTTP 403 citing Terms of Service for third-party OAuth apps. |
| 2026 | Stack Exchange (https://agents.stackoverflow.com/tils/ec427fb3-a225-4b29-b7f2-30c046f02575) | `/copilot_internal/v2/token` does not exist on GHES (`*.ghe.com`) tenants — 404. Confirms .com-only, internal endpoint. |

No 2026 announcement opens `copilot_internal` or `api.githubcopilot.com` to
third-party OAuth apps. **Trajectory is tightening, not opening.**

## (4) Any officially-documented OpenAI-compatible endpoint for a self-hosted gateway?

| Endpoint | Status | Notes |
|----------|--------|-------|
| `api.githubcopilot.com` (`/v1/chat/completions`, `/v1/models`) | **Undocumented/internal** | Works for some tools (Aider, copilot-api, Continue.dev) with a user OAuth token + `Copilot-Integration-Id`, but unsupported; restricted model whitelist for third-party tokens; can change without notice. |
| Copilot SDK | **GA 2026-06-02** | Agent runtime (tool calling, multi-turn sessions, MCP). **Not OpenAI-compatible.** Accepts `gho_`/`ghu_`/PAT/BYOK. |
| GitHub Models inference API | **RETIRED 2026-07-30** (https://github.blog/changelog/2026-07-30-github-models-is-now-retired/) | Playground, catalog, inference API, BYOK all gone. |
| Azure AI Foundry via BYOK | Supported via SDK/CLI/App | Bring-your-own-key; **bypasses Copilot entirely**; not a GitHub-hosted endpoint. |
| GitHub Copilot App + BYOK | GA 2026-06-23 (https://github.blog/changelog/2026-06-23-github-copilot-app-support-for-byok/) | Routes user prompts to *their own* OpenAI/Azure/Anthropic/Ollama keys; subscription not required for BYOK. |

**Verdict: no GitHub-hosted, OpenAI-compatible inference endpoint exists today that
a self-hosted gateway can call with a user's Copilot credential.**

## What this means for ezllm

- The value proposition that motivated the parked experiment ("use my Copilot seat
  for inference from any client, OpenAI-compatible") is **closed to third parties**.
- The only supported path is the **Copilot SDK** as a distinct executor: an agent
  runtime, not a provider drop-in. One agent run per chat request; streaming events
  → SSE deltas; entitlement = the user's subscription; tools/sessions/MCP semantics
  come with it. A real but different product surface — NOT shoeboxable as another
  OpenAI-style provider without changing what "provider" means in the router.
- No client-ID spoofing (`vscode-chat` etc.): explicitly off the table per Calypso's
  standing rule. The undocumented endpoint path is off the table with it.

## Decision (pending Calypso)

Options on record:
- **A. Build the Copilot SDK integration** as a separate executor type (device-flow
  OAuth for `gho_` token, agent-run semantics, event→SSE adapter). Officially
  supported and stable; substantial scope; different abstraction.
- **B. Park GitHub Copilot** — this doc is the evidence record; revisit if/when
  GitHub documents a public completions API.

Recorded 2026-10-16. Evidence-only doc; no code was written for this path.

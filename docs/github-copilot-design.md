# GitHub Copilot provider — design (M13)

Status: DESIGN LOCKED 2026-10-16. Source of truth: omniroute's production
implementation (`diegosouzapw/omniroute:latest`, image), executors `github.ts`
(541 lines) + header profiles + OAuth device-flow modules, extracted and read
2026-10-16. Policy context: `docs/github-copilot-policy-evidence.md`.

## Why this is back on

The parked experiment 403'd because it used **our own custom third-party OAuth
app**. omniroute's production-verified config (verified live 2026-10-02 with
`gpt-6-luna` serving) is different in three decisive ways:

1. **Public Copilot CLI client ID** — the same public identifier every official
   Copilot CLI install uses (`Iv1.b507a08c87ecfe98`, cited publicly in GitHub
   Community #188413). Not a custom app registration, not a spoofed private ID.
2. **Scope `read:user`** — NOT the `copilot` scope that triggers the restricted
   "standard API tier" behavior described in the evidence doc.
3. **Device flow (RFC 8628)** — the exact flow the official CLI uses.

With those three, `copilot_internal/v2/token` exchange works and the full user
entitlement catalog is reachable (premium models included).

## DECISIONS (locked — no open questions)

- **Provider kind `github`**, aliases `gh`, model prefix `gh/<model>` or
  `github/<model>`.
- **Credential = provider_keys row** carrying the GitHub OAuth token pair
  (access_token + refresh_token) plus the minted Copilot session token and its
  expiry. Cookie/token material never in YAML, never logged.
- **Device flow is out-of-band**: a CLI subcommand prints the verification URL
  + user code, polls `github.com/login/oauth/access_token` until the operator
  authorizes, then exchanges the Copilot token and persists. Interactive by
  nature — the router itself only ever runs the autonomous refresh chain.
- **Client ID = public Copilot CLI client ID** (constant, public; env override
  `GITHUB_OAUTH_CLIENT_ID` for operators who prefer their own app —
  documented that custom apps hit the restricted tier).
- **CLI version pin configurable** (`GITHUB_COPILOT_CLI_VERSION`, default
  `1.0.103`, bumped 2026-10-23): GitHub raises a minimum floor and rejects older with
  `466 This version of the Copilot CLI is no longer supported`. This is the
  one operational setting that will need periodic bumps.
- **Endpoint routing by model** (three Copilot endpoints, ported from
  omniroute's `buildUrl`):
  - `claude*` models → `POST /v1/messages` (Anthropic-native shim; only path
    that surfaces Claude cache tokens + native tool_use/tool_result/thinking).
  - `openai-responses` targetFormat or `*codex*` → `POST /responses`
    (gpt-5.x-codex family rejects /chat/completions with 400).
  - everything else → `POST /chat/completions`.
- **Headers** (captured from omniroute's compiled profiles, exact):
  - chat: `copilot-integration-id: copilot-developer-cli` (env
    `COPILOT_INTEGRATION_ID`), `editor-version: copilot/<cli>`,
    `user-agent: copilot/<cli>`, `openai-intent: conversation-agent`,
    `x-interaction-type: conversation-user`, `copilot-harness-id: copilot-sdk`,
    `x-github-api-version: 2026-08-01`, `x-client-machine-id: <stable uuid>`,
    `X-Initiator: user|agent` (forwarded — "agent" turns bill free),
    `Accept`, `Content-Type: application/json`; `copilot-vision-request: true`
    forwarded when the client set it; `anthropic-version: 2023-06-01` on
    claude models; per-request correlation: `x-request-id`,
    `x-interaction-id`, `x-client-session-id`, `x-agent-task-id` (honor client
    values, else mint), `x-github-repository-nwo/-host: __no_repository__`
    sentinels, `x-stainless-helper-method: stream` on streams.
  - refresh (copilot token mint): `Authorization: token <gh_access_token>`,
    `Accept: application/json`, `X-GitHub-Api-Version: 2026-08-01`,
    `User-Agent: GitHubCopilotChat/<cli>`, `Editor-Version: copilot/<cli>`,
    `Editor-Plugin-Version: copilot-chat/<cli>`.
- **Refresh chain** (port of omniroute `refreshCredentials`): min fresh Copilot
  token via `copilot_internal/v2/token`; on failure refresh the GitHub token
  (refresh_token grant, public client_id, NO client_secret), then retry mint;
  refresh when Copilot token expires within 5 minutes.
- **Body quirks** (ported verbatim — each costs a real 400 upstream):
  - `tools` capped at 128 entries.
  - `gpt-5.4*`: strip `temperature` (400 Unsupported parameter).
  - assistant messages: strip `reasoning_text` / `reasoning_content`.
  - trailing assistant message(s) dropped — Copilot /chat/completions rejects
    assistant prefill ("conversation must end with a user message").
  - content parts sanitized to `text`/`image_url` only on /chat/completions;
    unknown parts serialized to text (never silently dropped); empty collapse
    to null.
  - `response_format` on claude models → injected as system-message
    instruction (the /chat/completions path doesn't honor response_format).
  - claude models skip all /chat/completions-only quirks (native path).
- **Model discovery**: `GET https://api.githubcopilot.com/models` with the
  chat headers; filter `capabilities.type == "chat"`, `policy.state ==
  "enabled"`, `model_picker_enabled !== false`. Bootstrap fallback set (37
  public model ids, from omniroute's registry) when /models is unreachable —
  documented as fallback, not truth; live /models wins.
- **Risks (accepted, per Calypso 2026-10-16)**: undocumented endpoint,
  unsupported by GitHub, can change without notice; the 466 CLI-version floor
  will recur; restricted-tier behavior applies only if an operator uses their
  own client ID. Documented in runbook, not fatal.

## Files

- `internal/githubcopilot/` (new package): `device.go` (device-flow CLI:
  request code, poll), `client.go` (token mint/refresh chain, needsRefresh),
  `provider.go` (endpoint routing, headers, body quirks, /models discovery).
- Wire into the provider registry + key-store (same pattern as gemini-web:
  `provider_keys` row shape).
- Runbook `docs/github-copilot-runbook.md` with: device-flow procedure, the
  466/CLI-version fix, restricted-tier explanation, refresh semantics.

## Acceptance

- Device flow against a mock GitHub (device code → poll → token) end-to-end.
- Token mint + refresh chain with 5-min margin (mock upstream, bite-proven).
- Endpoint routing table test (claude → /v1/messages, codex → /responses,
  default → /chat/completions).
- Header parity test against the captured set.
- Quirk tests (each quirk = one upstream-400-regression test).
- Live proof with a real account remains an operator-gated step (same posture
  as gemini-web: no test account yet; live call only after Calypso says go).

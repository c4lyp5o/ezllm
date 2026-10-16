# github-copilot provider — operator runbook (M13)

GitHub Copilot through the **official Copilot CLI's public client id** and
device flow. Policy context: `docs/github-copilot-policy-evidence.md`
(custom third-party OAuth apps get a restricted tier; the CLI's public
identity with `scope=read:user` is the working path — verified against
production on 2026-10-16: the device-code request succeeds).

## Posture

- **No client-id spoofing.** We use GitHub's own public Copilot CLI client
  id (`Iv1.b507a08c87ecfe98`) with `read:user`, exactly like the CLI. We do
  NOT register a custom third-party app (restricted tier / 403), and we do
  NOT forge the VS Code / private client identities.
- **Undocumented inference surface.** `api.githubcopilot.com` is not a
  public API; GitHub can change it without notice. Both risks are accepted
  by Calypso (2026-10-16: "I understand the risk and we must move forward").
- **Credential = the account.** The stored session row carries your GitHub
  OAuth token pair — live account access. Keep it in the encrypted key store
  only, never in chat, logs, or YAML.

## Adding an account

**From the dashboard (no terminal — the phone path):**

1. Providers → Add provider → provider type **GitHub Copilot** → *Start
   GitHub login*. The dashboard shows `https://github.com/login/device`
   and a code; authorize it on any device.
2. The server polls, mints and stores the OAuth session itself — the
   session JSON never reaches the browser. When the status reads
   `authorized (@you)`, hit **Save & validate key**.
3. The key add names the *device handle* instead of a pasted key
   (`device_handle` on `POST /admin/accounts/{id}/keys`); the server
   resolves it, runs the normal registration test and consumes the handle.
   A failed test leaves the handle alive for a retry; closing the modal
   cancels it.

Routes: `POST /admin/github/device` (start), `GET
/admin/github/device/{handle}` (one poll per tick, statuses `pending |
authorized | expired | error`), `DELETE /admin/github/device/{handle}`
(cancel). Registry is in-memory, TTL 30 minutes, no schema.

**From a terminal (the CLI path):**

1. Run the interactive flow:
   ```
   ezllm github-login
   ```
   It prints `https://github.com/login/device` and a code; authorize in the
   browser. On success it verifies the Copilot token mint, lists your
   catalog (entitlement is per-account), and prints the session JSON.
2. Store that JSON as the account's provider key, with kind
   `github-copilot` and base URL `https://api.githubcopilot.com` (pasting
   it into the same Add-provider form works).
3. Registration runs the auth oracle (`GET /models` through the normal
   mint path): a dead credential fails as 401 bad_credentials.

## Model routing (automatic)

| Requested model | Endpoint | Why |
|---|---|---|
| `claude*` | `POST /v1/messages` (Anthropic-native shim) | Only Copilot path with cache tokens + native tool_use/tool_result/thinking |
| `*codex*` / openai-responses | `POST /responses` | Codex models 400 on /chat/completions |
| everything else | `POST /chat/completions` | Default |

Pin the per-model surface in the model_rules table if you want the
router to translate a client's wire for a specific model (e.g. force the
Anthropic surface for `claude-opus-4.6`).

## Day-2 operations

- **Token lifecycle is automatic**: the adapter mints a Copilot session
  token per account (in-memory, ~30 min life) and refreshes on a 5-minute
  margin; on mint failure it runs the GitHub refresh-token grant and mints
  again.
- **CLI version floor**: GitHub raises a minimum `editor-version`; older
  clients get `466 This version of the Copilot CLI is no longer supported`.
  Fix = set `GITHUB_COPILOT_CLI_VERSION=<current @github/copilot version>`
  (check `npm view @github/copilot version`) and restart. No code change.
- **Rotated refresh tokens are not persisted** by this adapter: if GitHub
  invalidates the stored refresh token, refreshes start failing with a 400
  — re-run `ezllm github-login` to re-authorize. (Copilot tokens are
  short-lived and always re-minted, so normal operation never hits this.)
- **Restricted-tier note**: if an operator sets `GITHUB_OAUTH_CLIENT_ID`
  to their own app, GitHub routes them to the stable-model-only tier. The
  default public CLI id does not.

## Failure cards

| Symptom | Meaning | Action |
|---|---|---|
| 401 bad_credentials at key test | Session dead / not entitled to Copilot | Re-run `ezllm github-login`; check the account has a Copilot seat |
| 403 at mint ("Bad credentials") | GitHub token invalid or app not entitled | Same |
| 400 `Unsupported parameter: 'temperature'` | gpt-5.4 quirk (auto-stripped) | Fixed in the adapter; file a bug if it leaks through |
| 400 "conversation must end with a user message" | assistant prefill (auto-dropped) | Fixed in the adapter |
| 400 `does not support Responses API` | a Claude/Gemini model routed to /responses | Routing bug; claude* always goes to /v1/messages |
| 466 (CLI version) | Version floor raised | Set GITHUB_COPILOT_CLI_VERSION, restart |

## Scope

This milestone ships the OpenAI/Anthropic/Responses passthrough with the
official auth path. Quota reads are unsupported (Copilot exposes no quota
endpoint on this surface — the adapter returns unsupported rather than
inventing numbers). Vision and non-chat capabilities pass through on the
wires that support them.

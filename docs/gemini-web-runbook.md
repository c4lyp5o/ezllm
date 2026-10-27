# gemini-web provider — operator runbook (M12)

Unofficial surface. This provider drives Google Gemini's **web** protocol
(gemini.google.com) with a Google-account cookie. Everything below is the
honest posture: what breaks, how it breaks, and what the operator does.

## Risk posture (accepted by Calypso, 2026-10-15)

- **Cookie material is live account access.** `__Secure-1PSID` +
  `__Secure-1PSIDTS` together are the account (not a scoped API key). Store
  them ONLY as a provider_keys row (encrypted at rest; the master key is a
  0600 file outside the DB). Never in YAML, never in a chat, never in a log.
  If a cookie leaks, rotate it by signing the Google account out of sessions.
- **Automation can get an account locked.** Google frowns on scripted use of
  the consumer web app. Treat a lock as a real possibility, not a theory.
- **Use a test account.** A secondary Google account with a free Gemini tier
  is the recommended credential. Never your primary.
- **No fallback.** This provider does not silently degrade to the official
  API. If the cookie dies, the account goes 401 and requests fail — that is
  the intended, honest behavior.

## Adding an account

1. Sign into gemini.google.com in a browser with the **test** account.
2. DevTools → Application/Storage → Cookies → `https://gemini.google.com`:
   copy the values of `__Secure-1PSID` and `__Secure-1PSIDTS`.
3. Store as the account's provider key, one cookie per line:
   ```
   __Secure-1PSID=<value>
   __Secure-1PSIDTS=<value>
   ```
4. Register the account with kind `gemini-web`. The base URL is the sentinel
   `internal://geminiweb` (the router rewrites it to the in-process bridge —
   no real host to fill in).
5. The registration key test runs the zero-quota oracle: it scrapes the app
   page, runs `GET_USER_STATUS`, and gates the account on the account-status
   verdict (1000 = live). A dead cookie fails as 401 bad_credentials.
6. Models are discovered from the same call — the account's actual catalog
   and tier (personal vs Workspace) come from the cookie, so the operator
   picks real model names from what the account reports.

## Day-2 operations

- **Rotation is automatic.** `__Secure-1PSIDTS` rotates roughly every 10
  minutes upstream; the bridge rotates every 8 and persists the new cookie
  in place (same key row, refreshed hint). No operator action. If rotation
  fails, the previous cookie keeps working for a while; the next request retries.
  If saving a rotated cookie fails, a warning appears in the server log. Check
  database availability and storage; the bridge retries persistence on a later
  request. Temporary validation credentials do not overwrite a stored key.
- **Catalogs re-sync** from the same zero-quota call; the bridge caches for
  30 minutes.
- **Usage limit (1037)** surfaces as 429 with the upstream code preserved in
  the error. `1013` → 503 (transient), `1060` → 403 (IP temporarily blocked —
  usually a datacenter/IP reputation problem, not the account).
- **Model header drift (1050/1052)** means Google moved the protocol. The
  model JSPB header is built per request from the live catalog — a 1052 is a
  protocol change, filed as a bug, not an operator action.

## Failure cards (what the operator sees)

| Symptom | Meaning | Action |
|---|---|---|
| 401 bad_credentials | Cookie expired / signed out / wrong flavor | Re-export the cookie from the test account, replace the key row |
| 403 upstream 1060 | IP temporarily blocked (automation signature) | Wait / egress from a residential-ish IP; do not hammer |
| 429 upstream 1037 | Account usage limit reached | Wait for the quota window; consider another test account |
| 400 unsupported_feature | tools / response_format requested | Expected: the web protocol has neither. Route those to a gemini-openai account |
| 400 model_not_found | Model id not in this account's catalog | Pick from `GET /models` on the account (its real tier) |
| Stream aborts mid-turn | Upstream closed the frame stream | The turn commits nothing; retry. No phantom conversation state is kept |

## Scope fences (typed refusals — never half-protocols)

No function calling. No structured output. No image input. Requests asking
for these get a typed 400 `unsupported_feature`, per the design doc. The web
protocol also flattens conversation history into one prompt text (no message
array, no per-message roles) — fine for chat, lossy for agent scaffolding.

## Protocol drift

The wire is reverse-engineered (see `~/workspace/gemini-web-api-protocol-digest.md`
and `docs/gemini-web-design.md` for every ported index and endpoint). It WILL
drift. When it does:

1. Symptoms: `ErrNoAccessToken` on live cookies (init page shape changed) or
   upstream 1050/1052 (model header changed).
2. The digest cites the upstream Python library file-by-file; the ported code
   in `internal/geminiweb` mirrors it slot-for-slot.
3. Update the reference, re-port, re-run the mock suite (29 tests pin the
   construction; the mock frames are the contract).

## Live proof (operator-gated)

Everything in this milestone is proven against mocks that mirror the wire.
The first real-cookie call is deliberately NOT automated: run it when you
choose, from the box you choose, with the test account, and watch the ledger
for the status verdict. If Google changes the init page or the header
tomorrow, that call is where you learn it first.

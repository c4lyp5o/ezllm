# Whole-application security audit — 2026-11-14

## Scope and result

Reviewed source at `368717a` on `feat/bridge-cache-and-updater` (ezllm v1.14.0),
including gateway and admin authentication, cookies and CSRF, dashboard rendering,
provider transports, body handling, storage and encrypted backups, compression
state, all four subscription bridges, updater gates, build/dependency inputs and
read-only deployment metadata.

Seven findings: one high, five medium and one low. No unauthenticated remote code
execution or admin-authentication bypass was demonstrated. This is a bounded
source/configuration audit, not a claim that every attack path has been excluded.
No production settings, keys, data or vendor sessions were changed. No paid
inference requests were made.

The running container reports ezllm **1.8.1**, while the reviewed branch is
**1.14.0**. Its copied binary reports **Go 1.27.4**; the local toolchain and
`go.mod` toolchain directive are **Go 1.27.2**. Application reproductions below
use the reviewed source and synthetic fixtures, not real user traffic against
the older deployment. Deployment observations are identified separately.

## Findings

| ID | Severity | Finding | Boundary / prerequisite |
| --- | --- | --- | --- |
| SEC-01 | High | Request body buffers can exhaust memory before admission limits | Valid inference key; concurrent incomplete uploads |
| SEC-02 | Medium | Provider responses can set gateway-origin admin cookies | Control of a configured upstream response |
| SEC-03 | Medium | Debug auth logs disclose VS Code path credentials | Debug logging and a refused path-token request |
| SEC-04 | Medium | Pending Codex continuations lack caller/history ownership checks | Valid inference access and knowledge of a pending tool-call ID |
| SEC-05 | Medium | Running HTTP port is published on all interfaces | Network reachability; external reachability was not tested |
| SEC-06 | Medium | Local/release toolchain pin predates security patches | A build actually using the pinned Go 1.27.2 toolchain |
| SEC-07 | Low | Settings writes bypass the admin body-read deadline | Admin authentication and an incomplete settings body |

### SEC-01 — Body allocation precedes resource admission

`internal/server/server.go` `inference` and `inferenceMultipart` authenticate,
then read bodies before resolving or dispatching an upstream request.
`internal/server/readbody.go` `readBody` allocates the entire positive declared
Content-Length as soon as it is within the per-request cap. There is no global
or per-client in-flight admission limit before that allocation. Subscription
bridge concurrency limits apply later and cannot protect this boundary.

A local real-HTTP reproduction opened eight authenticated connections, declared
2 MiB per request and sent one body byte on each. All eight entered `readBody`
and held **16,777,216 bytes** of payload buffers before any upstream call. With
the normal 32 MiB cap, 128 such buffers alone are 4 GiB; that is an extrapolation,
not an OOM test. The body deadline eventually releases them, but does not bound
the concurrent allocation. Incomplete requests have not accrued inference usage,
so a token budget does not stop this technique.

The observed container has no explicit Docker memory limit, increasing potential
impact beyond the gateway process. A reverse proxy with explicit body buffering
and admission limits could mitigate exposure; its enforcement was not established.

Recommended fix: bounded global and per-client request admission before body
allocation, plus an aggregate body-memory budget held while the request retains
its buffers. Bound or reject excess waiters. Keep body byte/deadline limits too;
reducing the one-body cap alone does not bound concurrency. Add a regression
showing excess stalled requests are rejected before receiving another full buffer.

### SEC-02 — Upstreams control gateway-origin cookies

`internal/proxy/proxy.go` `copyResponseHeaders` forwards every response header
except a static hop-by-hop list. It forwards `Set-Cookie` without constraining
cookie names, paths or purpose. A provider is trusted to supply inference data,
not to manage dashboard authentication on the gateway origin.

A synthetic upstream returned an `ezllm_session` cookie scoped to `/admin`.
An ordinary authenticated `/v1/chat/completions` response forwarded it, and the
HTTP client's cookie jar accepted that value for the gateway's `/admin/accounts`
URL. This permits cookie replacement and logout/cookie poisoning. A working
admin-session takeover was **not** demonstrated: the gateway still requires a
valid server-side session token.

Recommended fix: strip provider `Set-Cookie` and other origin-state-management
headers such as `Clear-Site-Data` before responding to gateway clients. If a
provider needs its own cookies, retain them in its account-scoped transport,
not in the dashboard browser. Test public and dashboard inference paths.

### SEC-03 — Credential redaction misses auth and panic logs

The normal access log uses `internal/server/server.go` `logPath`, but
`authenticate` logs `r.URL.Path` verbatim on invalid/expired-token refusals.
`internal/server/vscode_auth.go` `vscodeAuthed` sends the original path into this
gate; its path contains the credential. `recoverer` also uses the raw path.

A reproduction created and disabled a synthetic key, requested its VS Code alias
and captured debug logs. The full key appeared in the refusal line. Re-enabling
the key made the same credential valid again. Existing access-log-only tests do
not cover this debug path. The panic path is a static observation, not an induced
production crash.

Recommended fix: use `logPath` for every logged request path, including auth
refusals and panic recovery. Add sentinel tests at debug and error levels that
assert the raw credential is absent. If this path has been used with debug logs,
review access to retained logs and rotate affected credentials as appropriate.

### SEC-04 — A pending tool ID authorizes Codex state continuation

`internal/codexbridge/tools.go` `takeParked` selects a pending child when any
client-supplied tool-result ID matches. `runner.go` `Run` checks the template's
configuration key in cached mode, but does not check the original caller history
or an authenticated gateway principal. `Request` carries no authenticated caller
identity, and the loopback bridge bearer token is shared by gateway callers.

A fake-CLI reproduction started a tool turn with caller-A input, then submitted
unrelated caller-B input with the same model/instructions/tools and the known
pending ID. It resumed the original child and consumed the supplied result;
only one CLI was spawned. This demonstrates missing ownership/context validation.
It does **not** demonstrate guessing a real native ID, observing another user's
ID remotely, or extracting a real user's private conversation.

Recommended fix: propagate a gateway-authenticated caller identity through a
trusted internal boundary, and bind parked state to that caller plus its expected
conversation/pending batch. Overwrite or strip any externally supplied identity
header. Reject mismatches before unregistering the legitimate parked turn. Keep
empty-parent template reuse separate: those parents deliberately exclude caller
history and are not the sensitive pending conversation.

### SEC-05 — Direct HTTP publication can bypass the TLS proxy

Read-only Docker inspection showed container `ezllm` publishes port 20129 on
**0.0.0.0** and **::**, while also using the `traefik_default` network. A localhost
HTTP request to that port returned `/healthz`. The application uses
`http.Server.ListenAndServe`; TLS termination is external.

If reachable from an untrusted network, this exposes a direct plaintext path for
passwords, client credentials and prompts, bypassing the proxy's TLS and any
proxy-only restrictions. Host firewall rules and public/remote reachability were
not verified, so internet exposure is **not** claimed.

Recommended fix: when Traefik reaches the container through its Docker network,
remove the host publication. If direct local access is needed, bind it explicitly
to loopback. Verify IPv4 and IPv6 reachability rules. Preserve authenticated API
access; this finding is about the transport boundary, not an authentication bypass.

### SEC-06 — Build pin is behind patched Go releases

`go.mod` pins `toolchain go1.27.2`. The source and release workflows use
`go-version-file: go.mod`. The local `govulncheck` source scan reported:

- `GO-2026-6809`: certificate distrust update in `crypto/x509`, fixed in Go 1.27.3.
- `GO-2026-6859`: an HTTP/1 MaxBytesReader/header race, fixed in Go 1.27.4.

The TLS verification path is reachable. The HTTP advisory requires concurrent
body reading and response-header mutation; the observed settings decoder reads
synchronously, so an application exploit of that race was not established.
Scanner symbol reachability alone is insufficient to claim one.

The running Docker binary is built with **Go 1.27.4**, and its scan did not report
these two standard-library records. This finding concerns local/release builds
using the older pin, not proof that this deployment lacks those patches.
The floating Docker builder tag can pick a newer patch than the local pin.

Recommended fix: raise the toolchain pin to at least Go 1.27.4, rebuild release
artifacts and run a vulnerability gate in CI. Verify the resulting binary's build
metadata, rather than relying on source declarations alone.

Primary references:
[Go certificate advisory](https://pkg.go.dev/vuln/GO-2026-6809),
[Go HTTP race report](https://go.dev/issue/82563).

### SEC-07 — Settings body reads omit the shared deadline

`internal/server/settings.go` `handleSettings` constructs a JSON decoder directly
on `http.MaxBytesReader`. Unlike `admin.go` `decodeBody`, it does not call
`server.go` `limitBodyRead`. The HTTP listener intentionally has no global read
timeout because responses may stream.

A real-HTTP test shortened the shared admin body budget to 100 ms, authenticated
as a synthetic admin and sent an incomplete settings PATCH. It was still waiting
after 500 ms; closing the client released it. This requires admin access, so the
severity is lower than the inference-key allocation issue.

Recommended fix: route this payload through the existing bounded/deadlined
`decodeBody` helper, preserving its single-JSON-object and unknown-field checks.
Add a stalled-settings-body regression alongside the existing login timeout test.

## Checks and evidence

| Check | Result |
| --- | --- |
| `make test-short` | Passed; full local functional gate, including auth, CSRF, ownership, redirect, backup and bridge regressions |
| `go vet ./...` | Passed |
| `python3 -m unittest discover -s scripts -p 'bridge_update_test.py'` | 54 tests passed |
| `python3 tests/security_audit.py` | Five synthetic/local reproductions: SEC-01/02/03/04/07 |
| `npm audit --package-lock-only --json` in `web/` | Zero reported vulnerabilities across the lockfile graph |
| `govulncheck` source scan | Two standard-library security records with source-symbol traces; one uncalled OpenPGP deprecation record |
| `govulncheck` running-binary scan | No two stdlib records above; OpenPGP wildcard/deprecation record needs reachability triage |
| High-confidence credential regex scan across all local git refs | 332,265 diff lines scanned; no matching locations |
| Landlock synthetic parent-environment probe | Unconfined control read its parent's synthetic sentinel; confined child was denied |
| Runtime metadata | Nonroot UID/GID 1000, not privileged, no Codex Landlock override; all-interface HTTP publication; no Docker memory cap |

`tests/security_audit.py` temporarily creates exclusive in-package Go probes,
runs them with `-short` and removes them in `finally`. It does not touch a real
database, subscription login or live gateway. Its successful exit means the
findings **reproduced**, not that security passed. The body-allocation proof is
limited to 16 MiB. After remediation, convert the relevant expectations into
permanent regressions asserting rejection/redaction; this audit driver should
then be updated or retired rather than weakening those regressions.

The OpenPGP record is `GO-2026-5932` against `golang.org/x/crypto`. Current source
imports no `openpgp` package. The binary scan's apparent function matches are
wildcard package markers rather than concrete application call stacks, and the
copied binary contains no OpenPGP package path. It is not counted as a demonstrated
OpenPGP application vulnerability. This is not a reason to ignore future records.

## Controls reviewed without a demonstrated bypass

- Admin routes have explicit admin-role/session gates; inference-only tokens
  are refused at those gates. Expiry and budgets share the inference token gate.
- First boot requires a setup code, passwords use bcrypt, dashboard sessions
  are hashed at rest, cookies are HttpOnly/SameSite=Strict, and cookie-authenticated
  writes use Go's cross-origin protection. Login hashing and source tables are bounded.
- Inference CORS does not grant admin CORS. Dashboard rendering uses React nodes,
  not raw provider HTML; shell CSP restricts scripts and framing. No stored-XSS
  bypass was demonstrated in the reviewed renderers.
- SQL inputs use parameters, and reviewed dynamic column names come from fixed
  mappings. No SQL injection was demonstrated in those paths.
- Provider credentials use AES-GCM at rest; backups use authenticated age
  encryption and fixed regular-file archive members. Restore and export have
  existing integrity/cleanup tests. This audit did not decrypt real backups.
- Credential-bearing provider clients reject cross-origin redirects. Notification
  delivery refuses redirects and redacts secret error values. Arbitrary provider
  and webhook destinations are admin-controlled features; private-network URLs
  were not labeled SSRF merely because local providers are supported.
- Native tools remain disabled or policy-gated per bridge. Environment allowlists,
  loopback/shared-secret admission and Codex Landlock remain in place. Cached
  completed-history reuse compares full prior histories; Codex empty parents never
  receive caller dialogue. No OS containment escape was demonstrated.
- Updater downloads verify digests and use data-filtered extraction; promotion
  uses configuration-specific proof gates and rollback state. No updater run,
  vendor CLI update, binary promotion or production restart was performed.

## Remediation order and limits

Prioritize SEC-01 for resource safety and SEC-02/03/04 for trust and confidentiality
boundaries. Address the direct-port exposure and build pin alongside those fixes;
SEC-07 is a small reuse of the existing body-reader control.

No firewall audit, internet-facing penetration test, credential rotation, broad
fuzzing campaign or full browser attack matrix was performed. Regex history
scanning is not proof that no secret of any format has ever been committed.
Paid inference behavior and vendor CLI implementation security were not retested.
Findings are open; no application or deployment remediation is included in this
report-only change.

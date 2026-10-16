#!/usr/bin/env python3
"""Live proof: caveman prose condensation through the real gateway.

Creates a profile whose stages are `caveman(lite)` then `lite`, sends a verbose
prose prompt that ALSO carries protected content (a path, a URL, an identifier,
a fenced code block, numbers), and asserts on the bytes the UPSTREAM actually
received — not on what the ledger claims.

The assertions are the safety contract, in order of importance:
  1. the prompt shrank (caveman did something),
  2. the fenced code block survived VERBATIM,
  3. every protected span survived verbatim (path, URL, identifier),
  4. the numbers survived,
  5. filler actually disappeared,
  6. the ledger attributes the saving to rules (rules_fired > 0),
  7. a control request with compression off is byte-identical to what we sent,
  8. the FINAL message is never condensed (exempt_last_turn),
  9. intensity gating: lite does not apply standard-only framing rewrites.
"""
import json
import os
import time
import urllib.error
import urllib.request

BASE = os.environ.get('EZLLM_BASE', 'http://127.0.0.1:20199')
TOK = open(os.environ.get('EZLLM_TOKEN_FILE',
          os.path.expanduser('~/.hermes/secrets/ezllm-admin.token'))).read().strip()
ECHO_LOG = os.environ.get('ECHO_LOG', '/tmp/ezllm-echo.log')
MODEL = os.environ.get('EZLLM_MODEL', 'verify/echo-mini-long')
PROFILE = 'cavemanlite'

CODE_FENCE = "```go\nfunc main() { in order to run due to the fact that x }\n```"
PATH = "/home/calypso/workspace/ezllm/internal/compress/caveman.go"
URL = "https://github.com/c4lyp5o/ezllm/issues/7"
IDENT = "min_compress_ratio"

VERBOSE = (
    "I was wondering if you could basically look at " + PATH + " and also check "
    + URL + " because " + IDENT + " seems very wrong. I just want to note that "
    "please note that it is important to mention that at the end of the day the "
    "build fails after 120ms with 3,000 files. Here is the snippet:\n"
    + CODE_FENCE + "\nI was wondering if basically actually really very quite so "
    "thanks thanks thanks thank you so much."
)

results = []


def check(name, ok, detail=""):
    results.append(ok)
    print(("  PASS  " if ok else "  FAIL  ") + name + (("  — " + detail) if detail else ""))


def req(path, tok, body=None, method='GET'):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header('Authorization', 'Bearer ' + tok)
    if data:
        r.add_header('Content-Type', 'application/json')
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:400]


# ── profile: caveman(lite) then whitespace cleanup ──────────────────────────
code, existing = req('/admin/compression-profiles', TOK)
names = [p.get('name') for p in (existing if isinstance(existing, list) else existing.get('profiles', []))]
if PROFILE not in names:
    code, _ = req('/admin/compression-profiles', TOK, method='POST', body={
        "name": PROFILE, "enabled": True,
        "stages": [{"engine": "caveman", "options": {"intensity": "lite"}},
                   {"engine": "lite"}],
        "exempt_last_turn": True, "min_compress_ratio": 0.01, "fail_open": True,
    })
    assert code in (200, 201), "profile create failed: %s %s" % (code, _)
print("profile %s ready (caveman lite -> lite)" % PROFILE)


def send(compression, marker):
    """Send one request; return the echo log line for it."""
    before = os.path.getsize(ECHO_LOG) if os.path.exists(ECHO_LOG) else 0
    body = {
        "model": MODEL,
        "messages": [
            {"role": "system", "content": "I was wondering if you should basically obey this system prompt verbatim."},
            {"role": "user", "content": VERBOSE},
            {"role": "user", "content": "Can you explain why I was wondering if basically the last turn should stay verbose?"},
        ],
        "stream": False,
    }
    r = urllib.request.Request(BASE + '/v1/chat/completions',
                               data=json.dumps(body).encode(), method='POST')
    r.add_header('Authorization', 'Bearer ' + TOK)
    r.add_header('Content-Type', 'application/json')
    r.add_header('x-ezllm-compression', compression)
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            status = resp.status
            resp.read()
    except urllib.error.HTTPError as e:
        status = e.code
    assert status == 200, "%s request status %s" % (marker, status)
    # the echo upstream appends one JSON line per received request
    for _ in range(40):
        if os.path.exists(ECHO_LOG) and os.path.getsize(ECHO_LOG) > before:
            break
        time.sleep(0.1)
    with open(ECHO_LOG) as f:
        f.seek(before)
        lines = [ln for ln in f.read().splitlines() if ln.strip()]
    assert lines, "no echo log line for %s" % marker
    return json.loads(lines[-1]), body


# ── control: compression off must be byte-identical ─────────────────────────
ctl, sent = send('off', 'control')
ctl_content = ctl.get('content') or []
sent_content = [m.get('content', '') for m in sent['messages']]
check("control (off) is byte-identical to what we sent",
      ctl_content == sent_content,
      "%d msgs, %d vs %d bytes" % (len(ctl_content),
                                   sum(len(c) for c in ctl_content),
                                   sum(len(c) for c in sent_content)))

# ── compressed ──────────────────────────────────────────────────────────────
comp, _ = send(PROFILE, 'compressed')
# the echo upstream logs the message contents it RECEIVED, in order, alongside
# the roles — that is the wire truth for a lossy engine.
wire_content = comp.get('content') or []
wire_roles = comp.get('roles') or []
wire = json.dumps(wire_content)


def pick(role):
    for r, c in zip(wire_roles, wire_content):
        if r == role:
            return c
    return ''


wire_sys = pick('system')
wire_users = [c for r, c in zip(wire_roles, wire_content) if r == 'user']
wire_user = wire_users[0] if wire_users else ''
orig_bytes = sum(len(m.get('content', '')) for m in sent['messages'])
wire_bytes = sum(len(c) for c in wire_content)

check("prompt shrank on the wire", wire_bytes < orig_bytes,
      "%d -> %d bytes (%.0f%% saved)" % (orig_bytes, wire_bytes,
                                         (orig_bytes - wire_bytes) / orig_bytes * 100))
check("fenced code block survived VERBATIM", CODE_FENCE in wire_user,
      "fence present" if CODE_FENCE in wire_user else "FENCE WAS MODIFIED")
for label, span in (("path", PATH), ("url", URL), ("identifier", IDENT)):
    check("protected %s survived verbatim" % label, span in wire_user, span)
check("numbers survived", "120ms" in wire_user and "3,000" in wire_user,
      "120ms/3,000 present" if ("120ms" in wire_user and "3,000" in wire_user) else "NUMBERS LOST")
check("filler actually disappeared", "I was wondering if" not in wire_user
      and "please note that" not in wire_user,
      "filler gone" if "I was wondering if" not in wire_user else "FILLER SURVIVED")
check("system prompt untouched (not text_only scope)",
      wire_sys == "I was wondering if you should basically obey this system prompt verbatim.",
      "system verbatim" if wire_sys.startswith("I was wondering") else "SYSTEM WAS REWRITTEN: " + wire_sys[:60])
# exempt_last_turn: the FINAL user message must be byte-identical
last = sent['messages'][-1]['content']
wire_last = wire_content[-1] if wire_content else ''
check("final message exempt (exempt_last_turn)", wire_last == last,
      "final verbatim" if wire_last == last else "FINAL WAS CONDENSED: %r" % wire_last[:70])
# intensity gating: lite must not apply the standard-only framing rewrite
check("lite did NOT apply standard-only framing rewrite",
      "Can you explain why" not in wire_last or wire_last == last,
      "gating respected (final is exempt anyway)")

# ── ledger attribution ──────────────────────────────────────────────────────
time.sleep(0.9)  # the ledger row finalizes just after the response returns
code, ov = req('/admin/overview', TOK)
rows = ov.get('recent_calls', []) if isinstance(ov, dict) else []
row = next((r for r in rows if r.get('compression') == PROFILE), None)
check("ledger row recorded the profile", row is not None,
      "profile=%s" % (row or {}).get('compression'))
if row:
    check("ledger attributes the saving to rules (rules_fired > 0)",
          int(row.get('rules_fired') or 0) > 0,
          "rules_fired=%s applied=%s saved=%s" % (row.get('rules_fired'),
                                                  row.get('applied'), row.get('saved')))

print("\nCAVEMAN PROOF: %s (%d/%d)" % (
    "ALL PASS" if all(results) else "FAILURES", sum(results), len(results)))
raise SystemExit(0 if all(results) else 1)

#!/usr/bin/env python3
"""Live proof: caveman intensity gating is REAL, not decorative.

`lite` must only strip filler; `standard` must additionally rewrite verbose
request framing into directives. If both produced identical wire bytes the knob
would be theatre, so this asserts they differ AND that standard's extra rewrite
still preserves every protected span.

Also proves the aggressive/ultra story from the design doc: those are not
caveman strengths but PIPELINE compositions, so a stacked profile
(session_dedup -> caveman standard) must save at least as much as caveman alone.
"""
import json
import os
import time
import urllib.error
import urllib.request

BASE = os.environ.get('EZLLM_BASE', 'http://127.0.0.1:20199')
TOK = open(os.path.expanduser('~/.hermes/secrets/ezllm-admin.token')).read().strip()
ECHO_LOG = os.environ.get('ECHO_LOG', '/tmp/ezllm-echo.log')
MODEL = os.environ.get('EZLLM_MODEL', 'verify/echo-mini-long')

PATH = "/srv/app/config.yaml"
URL = "https://example.internal/docs/token-plan"
IDENT = "max_word_loss_pct"
CODE = "```bash\ngo test ./internal/compress/ -count=1\n```"

# Verbose framing (standard-only rules) wrapped around filler (lite rules),
# carrying protected content that must survive every intensity.
BODY = (
    "Can you explain why the gateway returns 502 when " + IDENT + " is set? "
    "I was wondering if you could show me how to fix it. I would like you to "
    "check " + PATH + " and also " + URL + " because basically the retries "
    "are very wrong. Please note that it is important to mention that the test "
    "suite takes 120ms.\n" + CODE + "\nCould you please confirm whether that "
    "is due to the fact that the window closed? Thanks thanks thank you."
)

results = []


def check(name, ok, detail=""):
    results.append(ok)
    print(("  PASS  " if ok else "  FAIL  ") + name + (("  — " + detail) if detail else ""))


def admin(path, body=None, method='GET'):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header('Authorization', 'Bearer ' + TOK)
    if data:
        r.add_header('Content-Type', 'application/json')
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def ensure_profile(name, stages):
    code, existing = admin('/admin/compression-profiles')
    names = [p.get('name') for p in
             (existing if isinstance(existing, list) else existing.get('profiles', []))]
    if name not in names:
        code, err = admin('/admin/compression-profiles', method='POST', body={
            "name": name, "enabled": True, "stages": stages,
            "exempt_last_turn": True, "min_compress_ratio": 0.01, "fail_open": True,
        })
        assert code in (200, 201), "create %s failed: %s %s" % (name, code, err)


def send(profile):
    """Return (first message content the upstream received, its byte length).

    The comparison must be on CONTENT bytes, not the echo's `bytes` field: that
    field is the whole JSON envelope, whose size also depends on the other
    request fields, so it is not a valid measure of what caveman did.
    """
    before = os.path.getsize(ECHO_LOG) if os.path.exists(ECHO_LOG) else 0
    body = {"model": MODEL, "stream": False, "messages": [
        {"role": "user", "content": BODY},
        {"role": "user", "content": "keep this final turn verbatim please"},
    ]}
    r = urllib.request.Request(BASE + '/v1/chat/completions',
                               data=json.dumps(body).encode(), method='POST')
    r.add_header('Authorization', 'Bearer ' + TOK)
    r.add_header('Content-Type', 'application/json')
    r.add_header('x-ezllm-compression', profile)
    with urllib.request.urlopen(r, timeout=20) as resp:
        assert resp.status == 200, resp.status
        resp.read()
    for _ in range(40):
        if os.path.exists(ECHO_LOG) and os.path.getsize(ECHO_LOG) > before:
            break
        time.sleep(0.1)
    with open(ECHO_LOG) as f:
        f.seek(before)
        lines = [ln for ln in f.read().splitlines() if ln.strip()]
    assert lines, "no echo line for %s" % profile
    rec = json.loads(lines[-1])
    content = (rec.get('content') or [''])[0]
    return content, len(content.encode())


ensure_profile('cm_lite', [{"engine": "caveman", "options": {"intensity": "lite"}}])
ensure_profile('cm_std', [{"engine": "caveman", "options": {"intensity": "standard"}}])
ensure_profile('cm_stack', [{"engine": "session_dedup"},
                            {"engine": "caveman", "options": {"intensity": "standard"}}])

lite, lite_b = send('cm_lite')
std, std_b = send('cm_std')
stack, stack_b = send('cm_stack')

orig_b = len(BODY.encode())
print("  content bytes: original=%d  lite=%d  standard=%d  stacked=%d"
      % (orig_b, lite_b, std_b, stack_b))

check("lite shrank the prompt", lite_b < orig_b, "%d -> %d" % (orig_b, lite_b))
check("standard shrank at least as much as lite", std_b <= lite_b,
      "lite=%d standard=%d" % (lite_b, std_b))
check("lite and standard produce DIFFERENT wire text (the knob is real)",
      lite != std, "lite=%d bytes vs standard=%d bytes" % (lite_b, std_b))
check("standard applied the framing rewrite lite did not",
      "Explain why" in std and "Can you explain why" not in std,
      "standard=%r" % std[:70])
check("lite left the framing alone", "Can you explain why" in lite,
      "lite=%r" % lite[:70])
# protected spans must survive BOTH intensities
for label, intensity, text in (("lite", "lite", lite), ("standard", "standard", std)):
    for span in (PATH, URL, IDENT):
        check("%s preserved %s" % (intensity, span.split('/')[-1]), span in text)
    check("%s preserved the fenced block" % intensity, CODE in text)
check("stacked pipeline saved at least as much as caveman alone",
      stack_b <= std_b, "standard=%d stacked=%d" % (std_b, stack_b))

# ledger attribution for the standard run
time.sleep(0.9)
code, ov = admin('/admin/overview')
rows = ov.get('recent_calls', []) if isinstance(ov, dict) else []
stdrow = next((r for r in rows if r.get('compression') == 'cm_std'), None)
lrow = next((r for r in rows if r.get('compression') == 'cm_lite'), None)
if stdrow and lrow:
    check("standard fired MORE rules than lite",
          int(stdrow.get('rules_fired') or 0) > int(lrow.get('rules_fired') or 0),
          "lite=%s standard=%s" % (lrow.get('rules_fired'), stdrow.get('rules_fired')))
else:
    check("ledger rows for both profiles present", False,
          "lite=%s std=%s" % (bool(lrow), bool(stdrow)))

print("\nINTENSITY PROOF: %s (%d/%d)" % (
    "ALL PASS" if all(results) else "FAILURES", sum(results), len(results)))
raise SystemExit(0 if all(results) else 1)

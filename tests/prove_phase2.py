#!/usr/bin/env python3
"""M6.5 phase 2 live proof: headroom + lite through the REAL server process.

  1. control   (x-ezllm-compression: off)   -> baseline wire bytes
  2. compressed (profile p2lite: headroom -> lite)

The echo upstream logs the RECEIVED body, so the assertions below are wire
truth, not ledger claims:
  - byte count down,
  - has_headroom_marker: the columnar form reached the upstream,
  - trailing_ws / blank_runs gone (lite cleaned the prose).
Cross-check via /admin/overview: the ledger row names profile p2lite,
applied=true, saved>0.

Leaves the p2lite profile standing so the dashboard shows both stages.
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
PROFILES = []

def req(method, path, body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header('Authorization', 'Bearer ' + TOK)
    if data:
        r.add_header('Content-Type', 'application/json')
    for k, v in (headers or {}).items():
        r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

def echo_count():
    if not os.path.exists(ECHO_LOG):
        return 0
    return sum(1 for _ in open(ECHO_LOG))

def echo_last():
    with open(ECHO_LOG) as f:
        lines = [l for l in f if l.strip()]
    return json.loads(lines[-1])

def row(i):
    return ('{"id":%d,"sku":"inventory-row-%03d-alpha","qty":%d,"zone":"shelf-c"}'
            % (i, i, i * 3))

TOOL_PAYLOAD = "[" + ",".join(row(i) for i in range(1, 13)) + "]"

def body_msgs():
    return [
        {"role": "user", "content": "Inventory duty.   \n\n\n\nFlag every row below qty 10.\n"},
        {"role": "assistant", "content": None,
         "tool_calls": [{"id": "call_1", "type": "function",
                         "function": {"name": "get_stock", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "call_1", "content": TOOL_PAYLOAD},
        {"role": "user", "content": "Thanks - summarise."},
    ]

def request(comp_header):
    return req("POST", "/v1/chat/completions", {
        "model": "verify/echo-mini-long",  # echo-mini sits in its CLOSED rule window
        "messages": body_msgs(),
        "temperature": 0.2,
        "x_custom": "preserved",
    }, {"x-ezllm-compression": comp_header})

# ---- 1. profile -----------------------------------------------------------
_, body = req("GET", "/admin/compression-profiles")
if not body.lstrip().startswith("["):
    raise SystemExit("GET profiles failed: %s" % body[:200])
existing = {p.get("name") for p in json.loads(body)}
if "p2lite" not in existing:
    code, body = req("POST", "/admin/compression-profiles", {
        "name": "p2lite",
        "stages": [{"engine": "headroom"}, {"engine": "lite"}],
    })
    if code not in (200, 201):
        raise SystemExit("profile create failed: %d %s" % (code, body))
    print(" 201  profile p2lite (headroom -> lite)     created")
else:
    print(" 200  profile p2lite                        already exists")

# ---- 2. control -----------------------------------------------------------
mark = echo_count()
code, _ = request("off")
ctrl = echo_last() if echo_count() > mark else {}
print(" %3d  control  (compression off)             wire=%dB roles=%s"
      % (code, ctrl.get("bytes", -1), ctrl.get("roles")))

# ---- 3. compressed --------------------------------------------------------
mark = echo_count()
code, _ = request("p2lite")
comp = echo_last() if echo_count() > mark else {}
print(" %3d  p2lite   wire=%dB headroom_marker=%s trailing_ws=%s blank_runs=%s"
      % (code, comp.get("bytes", -1), comp.get("has_headroom_marker"),
         comp.get("trailing_ws"), comp.get("blank_runs")))
if comp.get("wire_head"):
    print("       wire head: %s..." % comp["wire_head"][:90])

# ---- 4. ledger cross-check ------------------------------------------------
# The ledger row is finalized just AFTER the response returns (usage tap),
# so give it a beat or we read the stub (applied=0, saved=0) of our own row.
time.sleep(0.8)
_, body = req("GET", "/admin/overview")
ov = json.loads(body)
row0 = ov["recent_calls"][0]
print("       ledger: compression=%s applied=%s tin=%s saved=%s"
      % (row0.get("compression"), row0.get("applied"),
         row0.get("tin"), row0.get("saved")))

# ---- verdict --------------------------------------------------------------
ok = True
def check(label, cond):
    global ok
    print("  %s  %s" % ("PASS" if cond else "FAIL", label))
    ok = ok and cond

check("upstream got a smaller body (%s < %sB)"
      % (comp.get("bytes"), ctrl.get("bytes")),
      comp.get("bytes", 0) < ctrl.get("bytes", 1))
check("headroom columnar form on the wire", comp.get("has_headroom_marker") is True)
check("lite removed trailing whitespace", comp.get("trailing_ws") is False)
check("lite removed blank runs", comp.get("blank_runs") is False)
check("ledger names p2lite", row0.get("compression") == "p2lite")
check("ledger applied=true", row0.get("applied") is True)
check("ledger saved > 0", (row0.get("saved") or 0) > 0)
check("preserved fields survived (temp/x_custom)",
      comp.get("preserved") == {"stream": None, "temperature": 0.2, "x_custom": "preserved"})
check("roles/pairing intact", comp.get("roles") == ["user", "assistant", "tool", "user"])

print("\n%s" % ("PHASE2 PROOF: ALL PASS" if ok else "PHASE2 PROOF: FAILURES"))
raise SystemExit(0 if ok else 1)

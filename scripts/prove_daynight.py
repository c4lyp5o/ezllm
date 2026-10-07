"""Live day/night proof on the verify instance (local echo upstream only).

Sequence (combo daynight = hops [echo-mini-long, echo-mini], failover):
  1. day open / night closed  -> router must pick echo-mini
  2. both closed              -> 429 rule_refusal
  3. night open / day closed  -> router must pick echo-mini-long (the switch)
Leaves state 3 standing so the Rules page shows one open + one closed chip.
"""
import datetime, json, os, urllib.request, urllib.error, zoneinfo

# Instance targets — override for a non-default verify box:
#   EZLLM_BASE=http://127.0.0.1:20199 EZLLM_TOKEN_FILE=... python3 scripts/prove_daynight.py
BASE = os.environ.get('EZLLM_BASE', 'http://127.0.0.1:20199')
TOK = open(os.environ.get('EZLLM_TOKEN_FILE', os.path.expanduser('~/.hermes/secrets/ezllm-admin.token'))).read().strip()
ECHO_LOG = os.environ.get('ECHO_LOG', '/tmp/ezllm-echo.log')

def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header('Authorization', 'Bearer ' + TOK)
    if data:
        r.add_header('Content-Type', 'application/json')
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw.decode()

def echo_last(n=1):
    with open(ECHO_LOG) as f:
        return [json.loads(l) for l in f.readlines()[-n:]]

def infer(model):
    return req('POST', '/v1/chat/completions',
               {'model': model, 'messages': [{'role': 'user', 'content': 'ping'}], 'stream': False})

results = []

# ── combo ─────────────────────────────────────────────────────────────────
_, combos = req('GET', '/admin/combos')
if not any(c['name'] == 'daynight' for c in combos):
    s, body = req('POST', '/admin/combos', {
        'name': 'daynight', 'strategy': 'failover', 'sticky_idle_s': 1800, 'enabled': True,
        'hops': [{'account_id': 3, 'model_id': 'echo-mini-long', 'weight': 1, 'enabled': True},
                 {'account_id': 3, 'model_id': 'echo-mini', 'weight': 1, 'enabled': True}],
    })
    results.append(('combo daynight create', s, 'ok' if s in (200, 201) else str(body)))
else:
    results.append(('combo daynight create', 200, 'already exists'))

# ── rules, windows relative to now in the rule's tz ──────────────────────
TZ = zoneinfo.ZoneInfo('Asia/Kuala_Lumpur')
now = datetime.datetime.now(TZ)
hm = lambda d: d.strftime('%H:%M')
DAY_OPEN = (hm(now - datetime.timedelta(hours=1)), hm(now + datetime.timedelta(hours=1)))
CLOSED = (hm(now + datetime.timedelta(hours=3)), hm(now + datetime.timedelta(hours=4)))

def set_rule(model, win, note):
    return req('POST', '/admin/model-rules', {
        'account_id': 3, 'model_id': model, 'cap_tokens': 0, 'cap_window': 'monthly',
        'win_start': win[0], 'win_end': win[1], 'win_days': '',
        'win_tz': 'Asia/Kuala_Lumpur', 'enabled': True, 'note': note,
    })

def flip(results, label, model, win, note):
    s, body = set_rule(model, win, note)
    results.append((label, s, 'ok' if s in (200, 201) else str(body)))

# ── state 1: day open, night closed ──────────────────────────────────────
flip(results, 'rule echo-mini = day window', 'echo-mini', DAY_OPEN, 'day model')
flip(results, 'rule echo-mini-long = closed', 'echo-mini-long', CLOSED, 'night model')
s, _ = infer('daynight')
up = echo_last()
picked = up[-1]['model'] if up else '?'
results.append(('state1 day wins', s, f'upstream model={picked}' + (' ✓' if picked == 'echo-mini' else ' ✗')))

# ── state 2: both closed -> refusal ──────────────────────────────────────
flip(results, 'rule echo-mini = closed too', 'echo-mini', CLOSED, 'day model')
s, body = infer('daynight')
msg = json.dumps(body)[:160] if isinstance(body, dict) else str(body)[:160]
refused = s == 429
results.append(('state2 both closed', s, ('429 rule refusal ✓' if refused else '✗ expected 429: ') + msg))

# ── state 3: night opens (day stays closed) -> switch ────────────────────
flip(results, 'rule echo-mini-long = open', 'echo-mini-long', DAY_OPEN, 'night model')
s, _ = infer('daynight')
up = echo_last()
picked = up[-1]['model'] if up else '?'
results.append(('state3 night wins (switch)', s, f'upstream model={picked}' + (' ✓' if picked == 'echo-mini-long' else ' ✗')))

# ── direct-route control: a blocked model is blocked even alone ───────────
s, body = infer('verify/echo-mini')
msg = json.dumps(body)[:120] if isinstance(body, dict) else str(body)[:120]
results.append(('control blocked model alone', s, ('429 ✓' if s == 429 else '✗ ' + msg)))

print(f'now (rule tz): {now.strftime("%A %Y-%m-%d %H:%M %Z")}')
print(f'day window {DAY_OPEN} / closed window {CLOSED}')
print('-' * 72)
for label, status, detail in results:
    print(f'{status:>4}  {label:<32} {detail}')

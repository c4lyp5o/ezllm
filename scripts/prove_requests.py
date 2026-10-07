#!/usr/bin/env python3
"""Live proof: GET /admin/requests — the token dissection explorer.

Checks filter semantics against the real ledger (every echo call reports
tin=42, error rows carry tin=0, so the population splits cleanly):
  1. unfiltered listing + row shape (tin/tout/reasoning keys present),
  2. min_tin=1 keeps ONLY rows with tokens_in >= 1 (the 42s, errors gone),
  3. max_tin=0 keeps ONLY the zero-token (error) rows,
  4. sort=tin_desc puts summary.max_tin first,
  5. limit caps the page but never the summary,
  6. bad params 400 instead of silently ignoring.
"""
import json
import os
import urllib.error
import urllib.request
from typing import Any

# Both branches below return parsed JSON or an error snippet; the proof
# asserts status codes before indexing, so a loose Any is honest here.

BASE = os.environ.get('EZLLM_BASE', 'http://127.0.0.1:20199')
TOK = open(os.environ.get('EZLLM_TOKEN_FILE',
          os.path.expanduser('~/.hermes/secrets/ezllm-admin.token'))).read().strip()


def get(path) -> tuple[int, Any]:
    r = urllib.request.Request(BASE + path)
    r.add_header('Authorization', 'Bearer ' + TOK)
    try:
        with urllib.request.urlopen(r, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:200]


code, allr = get('/admin/requests')
assert code == 200, code
n_all = allr['summary']['count']
print(" 200  unfiltered: count=%d tin=%d tout=%d max_tin=%d rows=%d"
      % (n_all, allr['summary']['tin'], allr['summary']['tout'],
         allr['summary']['max_tin'], len(allr['rows'])))
row0 = allr['rows'][0]
for k in ('tin', 'tout', 'cread', 'reasoning', 'status', 'model', 'client', 'ts'):
    assert k in row0, 'row missing key %s' % k

code, pos = get('/admin/requests?min_tin=1')
assert code == 200, code
assert pos['summary']['count'] <= n_all
assert all(r['tin'] >= 1 for r in pos['rows']), 'min_tin=1 leaked a zero-token row'
print(" 200  min_tin=1: count=%d (unfiltered %d) — all rows tin>=1" % (pos['summary']['count'], n_all))

code, zero = get('/admin/requests?max_tin=0')
assert code == 200, code
assert all(r['tin'] == 0 for r in zero['rows']), 'max_tin=0 leaked a tokened row'
print(" 200  max_tin=0: count=%d — all rows tin==0 (error rows)" % zero['summary']['count'])

code, desc = get('/admin/requests?sort=tin_desc')
assert code == 200, code
first_tin = desc['rows'][0]['tin']
assert first_tin == desc['summary']['max_tin'], (first_tin, desc['summary']['max_tin'])
print(" 200  sort=tin_desc: rows[0].tin=%d == summary.max_tin ✓" % first_tin)

code, page = get('/admin/requests?limit=1')
assert code == 200, code
assert len(page['rows']) == 1 and page['summary']['count'] == n_all, \
    'limit must cap the page, not the summary'
print(" 200  limit=1: page=1 row, summary still %d ✓" % page['summary']['count'])

for bad in ('?min_tin=-5', '?min_tin=abc', '?sort=hax', '?limit=9999'):
    c, _ = get('/admin/requests' + bad)
    assert c == 400, '%s -> %d, want 400' % (bad, c)
print(" 400  bad params (min_tin=-5 / abc, sort=hax, limit=9999) all rejected ✓")

print("\nREQUESTS PROOF: ALL PASS")

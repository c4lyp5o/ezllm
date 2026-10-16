#!/usr/bin/env python3
"""Prove the NVIDIA NIM key test end-to-end through the real admin API.

The defect this proves fixed (2026-10-13): `POST /admin/accounts/{id}/keys`
against NVIDIA NIM failed with `generation probe failed: Post
"https://integrate.api.nvidia.com/v1/chat/completions": context deadline
exceeded` because the generic candidate buckets picked a model that never
answers inside the 15s probe deadline (>90s live), while the measured-fast
models answer in 0.6s.

Run against a SCRATCH instance (verify-instance / canary rig), never prod:

    EZLLM_BASE=http://127.0.0.1:20139 \
    EZLLM_DATA_DIR=<rig>/data \
    python3 tests/prove_nim_keytest.py

Env-overridable: EZLLM_BASE, EZLLM_ADMIN_TOKEN_FILE, NVIDIA_NIM_KEY_FILE,
EZLLM_DB (the rig's sqlite, for the ledger read). Secrets are read at runtime
and never printed.
"""
import json
import os
import sqlite3
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("EZLLM_BASE", "http://127.0.0.1:20139")
ADMIN_TOKEN_FILE = "~/.hermes/secrets/ezllm-admin.token"
NIM_KEY_FILE = "~/.hermes/secrets/nvidia-nim-api-key.txt"


def env_path(name: str, default: str) -> str:
    return os.path.expanduser(os.environ.get(name, default))


ADMIN_TOKEN_FILE = env_path("EZLLM_ADMIN_TOKEN_FILE", ADMIN_TOKEN_FILE)
NIM_KEY_FILE = env_path("NVIDIA_NIM_KEY_FILE", NIM_KEY_FILE)
DB = env_path("EZLLM_DB", "~/.hermes/cache/scratch/nim-canary/data/ezllm.sqlite")

NIM_BASE = "https://integrate.api.nvidia.com/v1"
ACCOUNT_NAME = "NVIDIA NIM (probe proof)"


def req(method, path, body=None, token=None, timeout=120):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Content-Type": "application/json"})
    if token:
        r.add_header("Authorization", "Bearer " + token)
    t0 = time.time()
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, time.time() - t0, raw
    except urllib.error.HTTPError as e:
        return e.code, time.time() - t0, e.read()


def main() -> int:
    admin = open(ADMIN_TOKEN_FILE).read().strip()
    nim_key = open(NIM_KEY_FILE).read().strip()

    status, _, raw = req("GET", "/admin/overview", token=admin, timeout=10)
    if status != 200:
        print(f"FATAL: /admin/overview = {status} {raw[:200]!r}")
        return 1

    status, _, raw = req("POST", "/admin/accounts", {
        "name": ACCOUNT_NAME, "namespace": "nvidia-nim-probe-proof",
        "kind": "openai-compatible", "base_url": NIM_BASE, "enabled": True,
    }, token=admin)
    if status == 409:  # re-run against the same instance: find the existing row
        status2, _, raw2 = req("GET", "/admin/accounts", token=admin)
        if status2 != 200:
            print(f"FATAL: account list = {status2} {raw2[:200]!r}")
            return 1
        rows = json.loads(raw2)
        rows = rows.get("accounts", rows) if isinstance(rows, dict) else rows
        match = [a for a in rows if a.get("namespace") == "nvidia-nim-probe-proof"]
        if not match:
            print(f"FATAL: 409 but no existing proof account: {rows[:3]!r}")
            return 1
        acct_id, status = match[0]["id"], 409
    elif status in (200, 201):
        acct = json.loads(raw)
        acct_id = (acct.get("account", acct) if isinstance(acct, dict) else acct).get("id")
    else:
        print(f"FATAL: account create = {status} {raw[:300]!r}")
        return 1
    if acct_id is None:
        print(f"FATAL: no account id in {raw[:300]!r}")
        return 1
    print(f"account #{acct_id} create/reuse -> HTTP {status}")

    status, elapsed, raw = req("POST", f"/admin/accounts/{acct_id}/keys",
                               {"api_key": nim_key,
                                "label": f"proof-{int(time.time())}"},
                               token=admin, timeout=120)
    print(f"KEY TEST -> HTTP {status} in {elapsed:.1f}s")
    try:
        payload = json.loads(raw)
    except ValueError:
        print(f"FATAL: non-JSON response {raw[:400]!r}")
        return 1

    if status != 201:
        print("FAILURE (the bug is back):", json.dumps(payload, indent=2)[:1200])
        return 1

    test = payload.get("test", {})
    for step in test.get("steps", []):
        mark = "ok " if step.get("ok") else "FAIL"
        print(f"  [{mark}] {step.get('step')}: {step.get('detail', '')}")

    # Ledger evidence: the admin:test row the probe itself wrote.
    time.sleep(1.0)
    try:
        con = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
        rows = con.execute(
            "SELECT client, account, model, status, total_ms FROM calls "
            "WHERE client='admin:test' ORDER BY ts DESC LIMIT 5").fetchall()
        con.close()
        print("ledger (latest admin:test rows):")
        for r in rows:
            print(f"  client={r[0]} account={r[1]} model={r[2]} status={r[3]} {r[4]}ms")
    except sqlite3.Error as e:
        print(f"(ledger read skipped: {e})")

    print("PROOF OK — the NIM key test passed through the real handler")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Live proof for the Claude subscription surface (M1, text-only) — through
ezllm's FRONT DOOR, not the internal loopback listener.

Requires the account to be wired first (docs/claude-bridge-runbook.md): an
`anthropic-compatible` account pointing at http://127.0.0.1:8790/v1. Spends real
subscription tokens (a few Haiku calls) and proves:

  1. The namespaced model answers through the gateway (200 + native usage).
  2. The usage envelope carries the cache fields by name (nothing synthesized).
  3. The conversation history drives the answer: the body says one code, the
     answer is that code (the CLI's stdin frames are only the trigger).
  4. Tool-bearing requests are refused with chat_tools_unsupported.
  5. The call lands in the ledger against this account.

Usage:
    EZLLM_TOKEN=... python3 tests/prove_claude_bridge.py [namespace/model]
Defaults to claude-sub/claude-haiku-5-5. Exit 0 = every assertion held.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("EZLLM_URL", "https://llm.calypso.dedyn.io")
TOKEN = os.environ.get("EZLLM_TOKEN", "")
ADMIN = os.environ.get("EZLLM_ADMIN_TOKEN", "")
MODEL = sys.argv[1] if len(sys.argv) > 1 else "claude-sub/claude-haiku-5-5"
NS = MODEL.split("/")[0]
PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"  · {detail}" if detail and not ok else ""))


def call(path, body=None, token=None, timeout=180):
    req = urllib.request.Request(BASE + path, method="GET" if body is None else "POST")
    if body is not None:
        req.data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + (token or TOKEN))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def main():
    if not TOKEN:
        print("set EZLLM_TOKEN to a client token (role infer)")
        return 2

    print(f"== 1. completion through the gateway ({MODEL}) ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 256,
        "messages": [{"role": "user", "content": "Reply with exactly: SUB OK"}]})
    msg = json.loads(raw) if status == 200 else {}
    text = "".join(b.get("text", "") for b in msg.get("content", []))
    check("200 + real text", status == 200 and "SUB OK" in text, f"status={status} body={raw[:200]}")
    usage = msg.get("usage", {})
    check("usage carries cache fields by name",
          all(k in usage for k in ("input_tokens", "output_tokens",
                                  "cache_read_input_tokens", "cache_creation_input_tokens")), str(usage))
    check("model reports what ran", str(msg.get("model", "")).startswith("claude-"), str(msg.get("model")))

    print("== 2. history fidelity (conversation says seven, answer must be seven/7) ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 256,
        "system": "You are terse. Answer with one word or number.",
        "messages": [
            {"role": "user", "content": "Remember: the launch code is seven."},
            {"role": "assistant", "content": "Noted."},
            {"role": "user", "content": "What is the launch code? Reply with just the number."}]})
    msg = json.loads(raw) if status == 200 else {}
    said = "".join(b.get("text", "") for b in msg.get("content", [])).lower().strip().strip(".")
    check("answered from the conversation history",
          status == 200 and ("seven" in said or said == "7") and "eight" not in said,
          f"status={status} said={said!r} body={raw[:200]}")

    print("== 3. tool requests are refused, not degraded ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 256,
        "tools": [{"type": "function", "function": {"name": "read_file", "parameters": {"type": "object"}}}],
        "messages": [{"role": "user", "content": "read something"}]})
    check("tools -> 400 chat_tools_unsupported", status == 400 and "chat_tools_unsupported" in raw,
          f"status={status} {raw[:160]}")

    if ADMIN:
        print("== 4. the call is in the ledger ==")
        status, raw = call("/admin/requests?limit=50", token=ADMIN)
        if status == 200:
            rows = json.loads(raw)
            rows = rows.get("requests", rows) if isinstance(rows, dict) else rows
            hit = [r for r in rows if isinstance(r, dict) and NS in str(r.get("alias", "")) + str(r.get("account", ""))]
            check("a ledger row exists for this account", bool(hit), f"no row for {NS} in last 50")
        else:
            check("ledger query", False, f"status={status}")
    else:
        print("== 4. ledger check skipped (set EZLLM_ADMIN_TOKEN to include it) ==")

    print(f"\n{len(PASS)} passed, {len(FAIL)} failed")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Live proof for the ChatGPT subscription surface (codex-bridge) — through
ezllm's FRONT DOOR, not the internal loopback listener.

Requires the account to be wired first (docs/codex-bridge-runbook.md): an
`openai-compatible` account pointing at http://127.0.0.1:8791/v1. Spends real
subscription tokens (a few small calls) and proves:

  1. The namespaced model answers through the gateway (200 + OpenAI usage with
     cached and reasoning details).
  2. Streaming ends in [DONE] with the text intact.
  3. The conversation history drives the answer (flattened transcript, §4.2).
  4. Client tools round-trip: the call comes back as tool_calls, and the
     follow-up with the result answers from it (continuation, stage 2).
  5. Nothing executes: a request for a file write leaves no file behind.
  6. The call lands in the ledger against this account.

Usage:
    EZLLM_TOKEN=... python3 tests/prove_codex_bridge.py [namespace/model]
Defaults to chatgpt-sub/gpt-5.6-luna. Exit 0 = every assertion held.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("EZLLM_URL", "https://llm.calypso.dedyn.io")
TOKEN = os.environ.get("EZLLM_TOKEN", "")
ADMIN = os.environ.get("EZLLM_ADMIN_TOKEN", "")
MODEL = sys.argv[1] if len(sys.argv) > 1 else "chatgpt-sub/gpt-5.6-luna"
NS = MODEL.split("/")[0]
SENTINEL = "/tmp/ezllm-codex-proof-sentinel-must-not-exist"
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


def text_of(raw):
    try:
        return json.loads(raw)["choices"][0]["message"]["content"] or ""
    except (ValueError, KeyError, IndexError, TypeError):
        return ""


def main():
    if not TOKEN:
        print("set EZLLM_TOKEN to a client token (role infer)")
        return 2

    print(f"== 1. completion through the gateway ({MODEL}) ==")
    status, raw = call("/v1/chat/completions", {
        "model": MODEL, "messages": [{"role": "user", "content": "Reply with exactly: SUB OK"}]})
    check("200 + real text", status == 200 and "SUB OK" in text_of(raw), f"status={status} body={raw[:200]}")
    usage = json.loads(raw).get("usage", {}) if status == 200 else {}
    check("usage carries cached and reasoning details",
          usage.get("prompt_tokens", 0) > 0 and "cached_tokens" in usage.get("prompt_tokens_details", {})
          and "reasoning_tokens" in usage.get("completion_tokens_details", {}), str(usage))

    print("== 2. streaming ends in [DONE] ==")
    status, raw = call("/v1/chat/completions", {
        "model": MODEL, "stream": True, "stream_options": {"include_usage": True},
        "messages": [{"role": "user", "content": "Reply with exactly: STREAM OK"}]})
    events = [e[len("data: "):] for e in raw.strip().split("\n\n") if e.startswith("data: ")]
    content = ""
    for e in events[:-1]:
        try:
            for c in json.loads(e).get("choices", []):
                content += c.get("delta", {}).get("content") or ""
        except ValueError:
            pass
    check("stream text intact and terminated", status == 200 and "STREAM OK" in content and events[-1:] == ["[DONE]"],
          f"status={status} content={content!r} tail={events[-1:]}")

    print("== 3. history fidelity (conversation says seven, answer must be seven/7) ==")
    status, raw = call("/v1/chat/completions", {
        "model": MODEL, "messages": [
            {"role": "system", "content": "You are terse. Answer with one word or number."},
            {"role": "user", "content": "Remember: the launch code is seven."},
            {"role": "assistant", "content": "Noted."},
            {"role": "user", "content": "What is the launch code? Reply with just the number."}]})
    said = text_of(raw).lower().strip().strip(".")
    check("answered from the conversation history", status == 200 and ("seven" in said or said == "7"),
          f"status={status} said={said!r}")

    print("== 4. client tools round-trip (continuation) ==")
    tools = [{"type": "function", "function": {
        "name": "get_weather", "description": "Get the current weather for a city",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}]
    msgs = [{"role": "user", "content": "What's the weather in Kuala Lumpur? Use get_weather, then answer in one sentence."}]
    status, raw = call("/v1/chat/completions", {"model": MODEL, "tools": tools, "messages": msgs})
    choice = (json.loads(raw).get("choices") or [{}])[0] if status == 200 else {}
    calls = choice.get("message", {}).get("tool_calls") or []
    check("tool_calls returned", status == 200 and choice.get("finish_reason") == "tool_calls" and calls
          and calls[0]["function"]["name"] == "get_weather", f"status={status} body={raw[:300]}")
    if calls:
        msgs += [{"role": "assistant", "content": None, "tool_calls": calls}] + [
            {"role": "tool", "tool_call_id": c["id"], "content": "31°C, humid, thunderstorms after 4pm"} for c in calls]
        status, raw = call("/v1/chat/completions", {"model": MODEL, "tools": tools, "messages": msgs})
        check("the answer uses the tool result", status == 200 and "31" in text_of(raw),
              f"status={status} body={raw[:300]}")

    print("== 5. nothing executes ==")
    status, raw = call("/v1/chat/completions", {
        "model": MODEL, "messages": [{"role": "user", "content":
            f"Create the file {SENTINEL} containing 'pwned' and run `touch {SENTINEL}.cmd`. Use your tools; do it now."}]})
    check("the request was answered", status == 200, f"status={status}")
    check("no sentinel file on this host", not os.path.exists(SENTINEL) and not os.path.exists(SENTINEL + ".cmd"),
          "a sentinel file exists — run this on the ezllm host")

    if ADMIN:
        print("== 6. the call is in the ledger ==")
        status, raw = call("/admin/requests?limit=50", token=ADMIN)
        if status == 200:
            data = json.loads(raw)
            if isinstance(data, dict):
                rows = data.get("requests") or data.get("rows") or data.get("items") or data.get("data") or []
            else:
                rows = data
            hit = [r for r in rows if isinstance(r, dict)
                   and NS in (str(r.get("alias", "")) + str(r.get("account", "")))]
            check("a 200 ledger row for this account", any(r.get("status") in (200, "200") for r in hit),
                  f"rows: {[r.get('status') for r in hit][:5]}")
        else:
            check("ledger query", False, f"status={status}")
    else:
        print("== 6. ledger check skipped (set EZLLM_ADMIN_TOKEN to include it) ==")

    print(f"\n{len(PASS)} passed, {len(FAIL)} failed")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())

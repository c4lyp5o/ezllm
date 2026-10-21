#!/usr/bin/env python3
"""Live proof for the Claude subscription surface (M1 transport + M2 tools) — through
ezllm's FRONT DOOR, not the internal loopback listener.

Requires the account to be wired first (docs/claude-bridge-runbook.md): an
`anthropic-compatible` account pointing at http://127.0.0.1:8790/v1. Spends real
subscription tokens (a few Haiku calls) and proves:

  1. The namespaced model answers through the gateway (200 + native usage).
  2. The usage envelope carries the cache fields by name (nothing synthesized).
  3. The conversation history drives the answer: the body says one code, the
     answer is that code (the CLI's stdin frames are only the trigger).
  4. Tools (M2): a call returns as tool_use under the client's name, a
     tool_result drives the answer, tool_choice forces a tool, nothing executes,
     the OpenAI chat path returns tool_calls, and server tools are refused.
  5. The call lands in the ledger against this account.

Usage:
    EZLLM_TOKEN=... python3 tests/prove_claude_bridge.py [namespace/model]
Defaults to claude-sub/claude-haiku-5-5. Exit 0 = every assertion held.
"""
import json
import os
import subprocess
import sys
import tempfile
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


WEATHER = {"name": "get_weather", "description": "Get current weather for a city.",
           "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}
WRITE = {"name": "write_file", "description": "Write content to a file at an absolute path.",
         "input_schema": {"type": "object", "properties": {"path": {"type": "string"},
                                                           "content": {"type": "string"}},
                          "required": ["path", "content"]}}
SENTINEL = "/tmp/ezllm-proof-sentinel-must-not-exist"


def tool_uses(msg):
    return [b for b in msg.get("content", []) if b.get("type") == "tool_use"]


def tool_checks():
    print("== 3. a tool call comes back under the client's name ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 1024, "tools": [WEATHER],
        "messages": [{"role": "user", "content": "What's the weather in Paris right now? Use the tool."}]})
    msg = json.loads(raw) if status == 200 else {}
    uses = tool_uses(msg)
    check("200 + get_weather tool_use, stop_reason tool_use",
          status == 200 and msg.get("stop_reason") == "tool_use" and uses and uses[0].get("name") == "get_weather",
          f"status={status} body={raw[:240]}")
    check("native's synthetic tool_result does not leak", "No such tool available" not in raw, raw[:240])

    print("== 4. the tool result drives the answer ==")
    use_id = uses[0]["id"] if uses else "toolu_proof"
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 1024, "tools": [WEATHER],
        "messages": [
            {"role": "user", "content": "What's the weather in Paris right now? Use the tool."},
            {"role": "assistant", "content": [{"type": "tool_use", "id": use_id, "name": "get_weather",
                                               "input": {"city": "Paris"}}]},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": use_id,
                                          "content": "17C, light rain"}]}]})
    msg = json.loads(raw) if status == 200 else {}
    text = "".join(b.get("text", "") for b in msg.get("content", []))
    check("answer uses the tool_result", status == 200 and "17" in text, f"status={status} body={raw[:240]}")

    print("== 5. tool_choice forces a named tool ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 1024, "tools": [WEATHER],
        "tool_choice": {"type": "tool", "name": "get_weather"},
        "messages": [{"role": "user", "content": "Say hi."}]})
    msg = json.loads(raw) if status == 200 else {}
    check("forced get_weather", status == 200 and [u.get("name") for u in tool_uses(msg)] == ["get_weather"],
          f"status={status} body={raw[:240]}")

    print("== 6. nothing executes ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 1024, "tools": [WRITE],
        "messages": [{"role": "user", "content": f"Write 'hello' to {SENTINEL} using the write_file tool."}]})
    msg = json.loads(raw) if status == 200 else {}
    check("write_file comes back as a call, not an action",
          status == 200 and [u.get("name") for u in tool_uses(msg)] == ["write_file"], f"status={status} {raw[:240]}")
    container = os.environ.get("EZLLM_CONTAINER", "")
    if container:
        # docker cp needs a real destination (a device such as /dev/null is
        # refused, which would read as "absent" on every run). Only the
        # daemon's own "Could not find" counts as absence; any other failure fails.
        with tempfile.TemporaryDirectory() as dest:
            cp = subprocess.run(["docker", "cp", f"{container}:{SENTINEL}", dest],
                                capture_output=True, text=True)
        absent = cp.returncode != 0 and "Could not find the file" in cp.stderr
        check("sentinel absent inside the container", absent,
              f"{SENTINEL}: rc={cp.returncode} {cp.stderr.strip()[:160]}")
    else:
        print("  (set EZLLM_CONTAINER=ezllm to also check the sentinel inside the container)")

    print("== 7. Hermes path: OpenAI chat with tools returns tool_calls ==")
    status, raw = call("/v1/chat/completions", {
        "model": MODEL, "max_tokens": 1024, "parallel_tool_calls": False,
        "tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather.",
                                                    "parameters": WEATHER["input_schema"]}}],
        "messages": [{"role": "system", "content": "You are Max."},
                     {"role": "user", "content": "Weather in Paris? Use the tool."}]})
    choice = (json.loads(raw).get("choices") or [{}])[0] if status == 200 else {}
    calls = (choice.get("message") or {}).get("tool_calls") or []
    check("tool_calls + finish_reason tool_calls",
          status == 200 and choice.get("finish_reason") == "tool_calls"
          and calls and calls[0]["function"]["name"] == "get_weather", f"status={status} body={raw[:240]}")

    print("== 8. server tools are refused before any spawn ==")
    status, raw = call("/v1/messages", {
        "model": MODEL, "max_tokens": 256,
        "tools": [{"type": "web_search_20250305", "name": "web_search"}],
        "messages": [{"role": "user", "content": "search something"}]})
    check("server tool -> 400 chat_tools_unsupported", status == 400 and "chat_tools_unsupported" in raw,
          f"status={status} {raw[:160]}")


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

    tool_checks()

    if ADMIN:
        print("== 9. the call is in the ledger ==")
        status, raw = call("/admin/requests?limit=50", token=ADMIN)
        if status == 200:
            data = json.loads(raw)
            if isinstance(data, dict):
                rows = data.get("requests") or data.get("rows") or data.get("items") or data.get("data") or []
            else:
                rows = data
            hit = [r for r in rows if isinstance(r, dict)
                   and NS in (str(r.get("alias", "")) + str(r.get("account", "")))]
            check("a ledger row exists for this account", bool(hit), f"no row for {NS} in last 50")
            ok = [r for r in hit if r.get("status") in (200, "200")]
            check("a 200 row for this account", bool(ok), f"rows: {[r.get('status') for r in hit][:5]}")
        else:
            check("ledger query", False, f"status={status}")
    else:
        print("== 9. ledger check skipped (set EZLLM_ADMIN_TOKEN to include it) ==")

    print(f"\n{len(PASS)} passed, {len(FAIL)} failed")
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())

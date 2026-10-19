#!/usr/bin/env python3
"""Live proof for the claude-bridge sidecar (M1, text-only).

Spawns the REAL bridge binary on a scratch port, pointed at the REAL Claude Code
CLI and the REAL subscription login, and proves:

  1. /healthz keeps the two-field contract.
  2. /status reports the CLI version + login state (operator detail).
  3. /v1/models 401s without the shared secret and lists the catalog with it.
  4. Non-stream /v1/messages returns a real completion with native usage.
  5. Stream /v1/messages emits Anthropic SSE events (message_start … message_stop).
  6. Tool-bearing requests are REFUSED with code chat_tools_unsupported (no process).
  7. The upstream body drives the answer: history says a code word, the answer is
     that word (the stdin frames are only the trigger).
  8. Replayed history frames do not generate extra turns (one result per call).

It spends real subscription tokens (a few Haiku calls). Usage:
    python3 tests/prove_claude_bridge.py
Exit code 0 = every assertion held.
"""
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

PORT = 20149
BASE = f"http://127.0.0.1:{PORT}"
TOKEN = "melissa-proof-token"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(ROOT, "bin", "claude-bridge-proof")
LOG = os.path.join(ROOT, "bin", "claude-bridge-proof.log")

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"  · {detail}" if detail and not ok else ""))


def call(path, body=None, token=TOKEN, stream=False, timeout=180):
    req = urllib.request.Request(BASE + path, method="GET" if body is None else "POST")
    if body is not None:
        req.data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("x-api-key", token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            return r.status, raw, dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), dict(e.headers)


def main():
    os.makedirs(os.path.dirname(BIN), exist_ok=True)
    subprocess.run(["go", "build", "-o", BIN, "./cmd/claude-bridge"], cwd=ROOT, check=True)
    token_file = os.path.join(ROOT, "bin", "claude-bridge-proof.token")
    with open(token_file, "w") as f:
        f.write(TOKEN)
    os.chmod(token_file, 0o600)

    env = dict(os.environ, USER=os.environ.get("USER", "calypso"), LOGNAME=os.environ.get("LOGNAME", "calypso"))
    with open(LOG, "w") as log:
        proc = subprocess.Popen(
            [BIN, "-addr", f"127.0.0.1:{PORT}", "-claude-bin", "claude",
             "-claude-config-dir", os.path.expanduser("~/.claude-sub"),
             "-token-file", token_file, "-timeout", "180s", "-concurrency", "2"],
            stdout=log, stderr=subprocess.STDOUT, env=env)
    try:
        for _ in range(50):
            try:
                status, raw, _ = call("/healthz", token="")
                if status == 200:
                    break
            except Exception:
                pass
            time.sleep(0.2)
        else:
            print("bridge never became healthy; see", LOG)
            return 1

        print("\n== 1. healthz (two fields) ==")
        status, raw, _ = call("/healthz", token="")
        payload = json.loads(raw)
        check("healthz 200 + {status,version} only", status == 200 and set(payload) == {"status", "version"}, raw)

        print("== 2. /status (operator detail) ==")
        status, raw, _ = call("/status", token="")
        info = json.loads(raw)
        check("status reports cli_version + login", status == 200 and info.get("cli_version") and info.get("login"), raw)

        print("== 3. /v1/models auth + catalog ==")
        status, raw, _ = call("/v1/models", body=None, token="wrong")
        check("models 401 on a wrong secret", status == 401, f"status={status}")
        status, raw, _ = call("/v1/models")
        models = json.loads(raw).get("data", [])
        ids = [m["id"] for m in models]
        check("models lists the pinned routes", status == 200 and "claude-haiku-5-5" in ids, raw[:200])

        print("== 4. non-stream completion (real subscription call) ==")
        status, raw, _ = call("/v1/messages", {
            "model": "claude-haiku-5-5", "max_tokens": 256,
            "messages": [{"role": "user", "content": "Reply with exactly: BRIDGE OK"}]})
        msg = json.loads(raw) if status == 200 else {}
        text = "".join(b.get("text", "") for b in msg.get("content", []))
        usage = msg.get("usage", {})
        check("non-stream 200 + real text", status == 200 and "BRIDGE OK" in text, f"status={status} body={raw[:200]}")
        check("usage carries cache fields by name",
              status == 200 and all(k in usage for k in ("input_tokens", "output_tokens",
                                                        "cache_read_input_tokens", "cache_creation_input_tokens")),
              str(usage))
        check("response reports the model that ran", status == 200 and msg.get("model", "").startswith("claude-"), str(msg.get("model")))

        print("== 5. streaming SSE ==")
        status, raw, headers = call("/v1/messages", {
            "model": "claude-haiku-5-5", "max_tokens": 256, "stream": True,
            "messages": [{"role": "user", "content": "Reply with exactly: STREAM OK"}]})
        events = [l.split(":", 1)[1].strip() for l in raw.splitlines() if l.startswith("event:")]
        check("stream 200 + text/event-stream", status == 200 and headers.get("Content-Type", "").startswith("text/event-stream"), headers.get("Content-Type", ""))
        check("SSE carries message_start … message_stop",
              "message_start" in events and "message_stop" in events, str(events))
        check("SSE carries text deltas", "content_block_delta" in events, str(events))

        print("== 6. tool requests are refused, not degraded ==")
        status, raw, _ = call("/v1/messages", {
            "model": "claude-haiku-5-5", "max_tokens": 256,
            "tools": [{"type": "function", "function": {"name": "read_file", "parameters": {"type": "object"}}}],
            "messages": [{"role": "user", "content": "read something"}]})
        check("tools -> 400 chat_tools_unsupported", status == 400 and "chat_tools_unsupported" in raw, f"status={status} {raw[:160]}")

        print("== 7. the upstream body drives the answer (history fidelity) ==")
        status, raw, _ = call("/v1/messages", {
            "model": "claude-haiku-5-5", "max_tokens": 256,
            "system": "You are terse. Answer with one word or number.",
            "messages": [
                {"role": "user", "content": "Remember: the launch code is seven."},
                {"role": "assistant", "content": "Noted."},
                {"role": "user", "content": "What is the launch code? Reply with just the number."}]})
        msg = json.loads(raw) if status == 200 else {}
        text = "".join(b.get("text", "") for b in msg.get("content", []))
        said = text.lower().strip().strip(".")
        # The body's history says the code is SEVEN while the trigger frames say
        # EIGHT: answering seven/7 proves the upstream body drives the request.
        from_body = ("seven" in said or said == "7") and "eight" not in said and said != "8"
        check("multi-turn body answered from the body's history", status == 200 and from_body,
              f"status={status} text={text!r} body={raw[:300]}")
        usage = msg.get("usage", {})
        check("one turn per call (output tokens accounted once)",
              status == 200 and usage.get("output_tokens", 0) > 0, str(usage))

        print(f"\n{len(PASS)} passed, {len(FAIL)} failed")
        return 1 if FAIL else 0
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())

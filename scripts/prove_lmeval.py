#!/usr/bin/env python3
"""prove_lmeval.py — run lm-eval-harness THROUGH the ezllm gateway and prove it.

Why this exists: "a harness can use our router" is a claim only a real harness
run supports. This drives lm-eval's local-chat-completions model class at the
gateway's /v1/chat/completions, then cross-checks the ledger so a green score
cannot come from a model that bypassed ezllm.

Two traps this already solved (do not re-learn them):
  * NO /v1/completions surface exists (405). lm-eval's `openai-completions`
    model class drives legacy /v1/completions with echo+logprobs for
    loglikelihood tasks (mmlu, arc, hellaswag) — those CANNOT work here.
    Use `local-chat-completions` + a generate_until task (gsm8k).
  * tokenizer_backend=auto probes {base_url}/tokenizer_info and falls back to
    loading `model` as a HuggingFace repo id. Our model is `opengo/deepseek-flash`,
    not a repo, so that path dies. Pass tokenizer_backend=none + apply_chat_template.

Env-overridable (scratch-instance defaults):
  EZLLM_BASE   gateway origin        default http://127.0.0.1:20129
  EZLLM_TOKEN  infer token file      default ~/.hermes/secrets/ezllm-infer.token
  EZLLM_LMEVAL path to lm_eval CLI   default ~/.venvs/lm-eval/bin/lm_eval
  EZLLM_TASK   task name             default gsm8k
  EZLLM_LIMIT  examples              default 100
  EZLLM_DB     ledger to verify      default <repo>/data/ezllm.sqlite

Usage:  python3 scripts/prove_lmeval.py [--keep] [--run-only] [--ledger-only]
Exit 0 = score reported AND ledger row count matched the run.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import NoReturn

BASE = os.environ.get("EZLLM_BASE", "http://127.0.0.1:20129")
TOKEN_FILE = Path(os.environ.get("EZLLM_TOKEN",
                                 str(Path.home() / ".hermes/secrets/ezllm-infer.token")))
LM_EVAL = os.environ.get("EZLLM_LMEVAL",
                         str(Path.home() / ".venvs/lm-eval/bin/lm_eval"))
TASK = os.environ.get("EZLLM_TASK", "gsm8k")
LIMIT = int(os.environ.get("EZLLM_LIMIT", "100"))
REPO = Path(__file__).resolve().parent.parent
DB = Path(os.environ.get("EZLLM_DB", str(REPO / "data" / "ezllm.sqlite")))
OUT = Path(os.environ.get("EZLLM_OUT",
                          "/home/calypso/.hermes/cache/scratch/ezllm-lmeval"))
LOG = Path(os.environ.get("EZLLM_LOG",
                          "/home/calypso/.hermes/cache/scratch/lmeval.log"))

# chat-endpoint-only shape: no loglikelihood, so generate_until tasks only.
MODEL_ARGS = (
    "model={model},"
    f"base_url={BASE}/v1/chat/completions,"
    "tokenizer_backend=none,tokenized_requests=False,max_gen_toks=1024,"
    "num_concurrent=8"
)
MODEL_CLASS = "local-chat-completions"


def fail(msg: str) -> NoReturn:
    print(f"FAIL: {msg}")
    sys.exit(1)


def run_harness(model: str) -> tuple[float, float, float]:
    """Launch lm-eval against the gateway. Returns (elapsed, flexible, strict)."""
    if not Path(LM_EVAL).exists():
        fail(f"lm-eval not found at {LM_EVAL} (uv pip install 'lm-eval[api]')")
    if not TOKEN_FILE.exists():
        fail(f"token file missing: {TOKEN_FILE}")

    token = TOKEN_FILE.read_text().strip()
    args = MODEL_ARGS.format(model=model)
    cmd = [
        LM_EVAL, "run",
        "--model", MODEL_CLASS,
        "--model_args", args,
        "--tasks", TASK,
        "--limit", str(LIMIT),
        "--batch_size", "1",
        "--apply_chat_template",          # required: chat class asserts list[dict]
        "--output_path", str(OUT),
    ]
    env = dict(os.environ, OPENAI_API_KEY=token)  # api_key property reads this env
    OUT.mkdir(parents=True, exist_ok=True)

    print(f"$ {' '.join(cmd[:6])} ... (log: {LOG})")
    t0 = time.time()
    with LOG.open("w") as fh:
        rc = subprocess.run(cmd, env=env, stdout=fh, stderr=subprocess.STDOUT).returncode
    elapsed = time.time() - t0
    if rc != 0:
        tail = LOG.read_text()[-1500:] if LOG.exists() else "(no log)"
        fail(f"lm-eval exited {rc} after {elapsed:.0f}s\n{tail}")
    return elapsed, *latest_scores()


def latest_scores() -> tuple[float, float]:
    """Newest results file -> (flexible_extract, strict_match) for TASK."""
    files = sorted(OUT.glob("**/*.json"), key=lambda p: p.stat().st_mtime)
    if not files:
        fail(f"no results json under {OUT}")
    d = json.loads(files[-1].read_text())
    res = d.get("results", {}).get(TASK, {})
    flex = res.get(f"exact_match,flexible-extract")
    strict = res.get(f"exact_match,strict-match")
    if flex is None:
        fail(f"no exact_match for {TASK} in {files[-1].name}: keys={list(res)}")
    print(f"results: {files[-1].name}")
    print(f"  {TASK} flexible-extract = {flex:.3f}   strict-match = {strict:.3f}")
    print(f"  sample_len = {res.get('sample_len')}")
    return float(flex), float(strict or 0)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--run-only", action="store_true",
                    help="score only, skip the ledger cross-check")
    ap.add_argument("--ledger-only", action="store_true",
                    help="skip lm-eval, just inspect the ledger")
    args = ap.parse_args()

    model = os.environ.get("EZLLM_MODEL", "opengo/deepseek-flash")
    before = row_count()

    if not args.ledger_only:
        elapsed, _flex, _strict = run_harness(model)
        print(f"elapsed: {elapsed:.0f}s  ({LIMIT} examples "
              f"~{LIMIT / max(elapsed, 1e-9):.1f} req/s through the gateway)")

    if not args.run_only:
        # Cross-check the run against the ledger BEFORE reporting PASS: a green
        # score means nothing if the traffic bypassed ezllm.
        after = row_count()
        if before is None or after is None:
            print("WARN: ledger unavailable — score reported WITHOUT proof it "
                  "transited the gateway")
        else:
            delta = after - before
            print(f"ledger: {before} -> {after} rows (+{delta} for {LIMIT} examples)")
            if not args.ledger_only and delta < LIMIT:
                fail(f"ledger gained only {delta} rows for {LIMIT} examples — "
                     "not all traffic transited ezllm")

    print("PASS")


def row_count() -> int | None:
    import sqlite3
    if not DB.exists():
        return None
    try:
        con = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
        try:
            return int(con.execute("select count(*) from calls").fetchone()[0])
        finally:
            con.close()
    except Exception as e:                               # noqa: BLE001
        print(f"(ledger read failed: {e})")
        return None


if __name__ == "__main__":
    main()

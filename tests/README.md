# tests/ — live proofs

Python scripts that prove a feature against a **running** ezllm, end to end.
They are not part of `go test` (the Go suite is in-package `_test.go` files)
and most need a prepared instance: an echo upstream, a scratch database, or a
real provider key. Each script's docstring says what it proves and what it
needs. Standard library only (`python3`, no virtualenv) except
`prove_lmeval.py`, which drives lm-eval-harness.

## Environment

One convention across every script: a bare variable is a **value**, a
`_FILE` variable is a **path**.

| variable | meaning | default |
|---|---|---|
| `EZLLM_BASE` | gateway origin | per script, below |
| `EZLLM_TOKEN` | client/infer token value | — |
| `EZLLM_TOKEN_FILE` | token file (admin for the admin proofs, infer for `prove_lmeval.py`) | `~/.hermes/secrets/ezllm-admin.token` / `ezllm-infer.token` |
| `EZLLM_ADMIN_TOKEN` / `EZLLM_ADMIN_TOKEN_FILE` | admin token value / file, where a script needs both roles | — |
| `EZLLM_DB` | ledger to cross-check | per script |

Default origins are deliberate — each points at the instance the proof was
written against, so a bare run never hits production by accident:

| script | default origin | instance |
|---|---|---|
| `prove_requests.py`, `prove_phase2.py`, `prove_daynight.py`, `prove_caveman*.py` | `http://127.0.0.1:20199` | scratch instance with the echo upstream |
| `prove_nim_keytest.py` | `http://127.0.0.1:20139` | NIM canary instance |
| `prove_lmeval.py` | `http://127.0.0.1:20129` | local gateway |
| `prove_claude_bridge.py` | `EZLLM_URL`, default the public front door | spends real subscription tokens; needs `EZLLM_TOKEN` |

Exit 0 means every assertion in the script held.

```bash
python3 -m py_compile tests/*.py   # syntax check without running anything
```

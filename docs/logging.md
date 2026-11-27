# Read the logs

ezllm writes its log to the terminal (stdout), one line per event. There is no log file:
Docker, systemd or whatever starts ezllm keeps it. Every request is also recorded in the
dashboard's **Requests**, which is the full record; the log tells the story around it.

## See it

Pick the way you run ezllm:

| You run | Command |
|---|---|
| Docker | `docker logs ezllm --tail 100` |
| Docker Compose | `docker compose logs ezllm --tail 100` |
| systemd user service | `journalctl --user -u ezllm -n 100` |
| macOS | `tail -n 100 ~/ezllm/logs/ezllm.log` |
| Windows | `Get-Content "$HOME\ezllm\logs\ezllm.log" -Tail 100` |
| By hand | the terminal you started it in |

Add `-f` to follow it live (`-Wait` on Windows). To find one thing, add `| grep`:

```sh
docker logs ezllm 2>&1 | grep -E 'WARN|ERROR'
```

## Read a line

Each request writes one line like this:

```
2026-10-24 08:01:15.254 INFO  request method=POST path=/v1/chat/completions status=200 ms=25709 bytes=31568 client=my-app account=acme model=acme/model-name surface=chat alias=main trail="other/model-name — 429 rate_limited"
```

| Field | Meaning |
|---|---|
| `client` | The ezllm key's name |
| `account`, `model` | The provider account and model that answered |
| `surface` | The API the app used: `chat`, `anthropic` or `responses` |
| `alias` | The combo name, when the app used one |
| `trail` | Hops that failed before this one, and why. A `200` with a trail means a provider is having trouble |

The first line after a start says how the log is set up:

```
2026-10-24 09:07:07.823 INFO  console logging log_level=info format=console color=false
```

## Change it

| Setting | Values |
|---|---|
| `EZLLM_LOG_LEVEL` | `info` (default), `debug`, `warn`, `error` |
| `EZLLM_LOG_FORMAT` | `console` (default), or `json` for one JSON object per line, for a log collector |
| `EZLLM_LOG_COLOR` | `auto` (default: colour only in a terminal), `always`, `never` |

Set them where you start ezllm, then restart it.

| Level | What it shows |
|---|---|
| `debug` | Extra detail to chase one problem, such as refused keys and streams closing |
| `info` | Starts, requests, skipped or rested providers, budgets reached |
| `warn` | Something went wrong but ezllm carried on, such as a failover or a full bridge queue |
| `error` | ezllm could not do its job |

The log never has a key, a password, a prompt or an answer.

---

**How it works:** [logging decisions](design/logging-design.md)

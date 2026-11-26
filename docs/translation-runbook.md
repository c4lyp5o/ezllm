# Translation between APIs

Your app speaks one API: Chat Completions (`chat`), Anthropic Messages (`anthropic`) or
Responses (`responses`). Some models speak only another one. **Translation** rewrites the
request for the model, and the answer back for your app, so they still work together.

It is on by default. Otherwise ezllm sends requests exactly as your app wrote them, apart
from the model name.

## When a request is translated

Only when a test has shown the model cannot answer your app's API. Untested models get
your app's API, untranslated. A request is translated when:

- **A test showed the model cannot serve your app's API**, but can serve another. ezllm
  picks the first one the model can serve, in this order:

  | Your app uses | Tried in order |
  |---|---|
  | `anthropic` | `chat`, then `responses` |
  | `chat` | `responses`, then `anthropic` |
  | `responses` | `chat`, then `anthropic` |

- **The model is pinned** to another API ([pins](#pins)).
- **The model is on the `responses_models` list** ([below](#models-that-need-responses)).

This works for combos and for a direct `provider/model` ID. In a combo, a translated hop is
an ordinary hop: same place in the order, same retries.

## Test a model

ezllm learns what a model speaks by testing it. In **Providers**, open an account's models
and choose **Test all models**. The tests send small real requests, which your provider may
bill. The **Protocols** column then shows what each model can serve.

A `429`, a server error or a timeout during a test teaches nothing, so the old result
stays.

When a provider blocks tests, record the fact yourself with an admin key. `note` is
required:

```sh
curl -s -X PUT -H "Authorization: Bearer YOUR_ADMIN_KEY" -H 'Content-Type: application/json' \
  http://127.0.0.1:20129/admin/accounts/ACCOUNT_ID/model-protocol \
  -d '{"model":"model-name","proto_chat":true,"proto_anthropic":false,"note":"refuses /v1/messages"}'
```

Later tests do not change a fact you recorded. Send `{"model":"model-name","release":true}`
to hand it back to the tests.

## What changes when a request is translated

- **Requests** marks the row `⇄ translated`. It keeps your app's API and the model that
  answered.
- On translated rows, `tokens_in` includes cached tokens and `tokens_cached_read` is `0`.
  Rules that cap input tokens count them a little early.
- A feature that would change the answer in translation, such as an unsupported content
  type, is refused before anything is sent. It costs nothing, and a combo moves to its
  next hop.
- Anthropic `thinking` blocks are dropped. ezllm adds no `cache_control`; the provider's
  own prefix caching still works.
- A translated stream that stops early still shows `200`, as your app saw it. The row
  records `translated stream failed` or `translated stream truncated`.

## Pins

A pin ties a model to one API, set in **Providers**. With translation on, a request on
another of the three APIs is translated to the pinned one.

| Case | Answer |
|---|---|
| Pinned to an API a test showed it cannot serve | `400 wire_unsupported` |
| Pinned to a service (`embeddings`, …) and called on a chat API, or the other way | `400 pinned_surface` |
| Translation off, and the request's API is not the pinned one | `400 pinned_surface` |

## Models that need Responses

Some models refuse tools with reasoning on `chat` but handle both on `responses`. List them
in `config.yaml`, and their `chat` and `anthropic` requests go through Responses:

```yaml
translation:
  responses_models:
    - "acme/model-name"
    - "acme/model-family.*"
```

Each entry is a regular expression that must match the whole `provider/model`. A bare
`sol` matches nothing. An entry that is not a valid expression stops ezllm from starting.

List only models that need it: a model with a working `chat` path loses a little on the
Responses path. A model belongs here when its `chat` requests fail with a message such as
`Function tools with reasoning_effort are not supported … use /v1/responses`.

## Anthropic providers that count cache differently

For a Messages provider, ezllm reads `input_tokens` as not including cached tokens, as
Anthropic does. A provider that includes them reports too many prompt tokens. Set its
`usage_convention` to `total` with an admin key:

```sh
curl -s -X PATCH -H "Authorization: Bearer YOUR_ADMIN_KEY" -H 'Content-Type: application/json' \
  http://127.0.0.1:20129/admin/accounts/ACCOUNT_ID -d '{"usage_convention":"total"}'
```

## Turn it off

In **Settings** → **Gateway**, switch off **Protocol translation**. It applies to new
requests at once. To fix it from the host, set `EZLLM_TRANSLATION_ENABLED=false` and
restart.

Test results stay recorded. With translation off, a model a test showed cannot serve your
app's API is skipped.

---

**How it works:** [translation design](design/translation-design.md) ·
[universal wire design](design/universal-wire-design.md) ·
[Responses wire design](design/responses-wire-design.md)

# Use Claude.ai through its web sign-in

The **Claude.ai (web)** provider sends requests the way the claude.ai website does,
signed in with a browser cookie. It is unofficial. For a Claude plan, the
[Claude bridge](bridges/claude.md) is the supported way.

Before you start, know the risks:

- **The cookie is your account.** Anyone with it can use the account. ezllm stores it
  encrypted; never put it in `config.yaml`, a chat or a log.
- **Scripted use can get the account locked.** Use a test account, never your main one.
- **Cloudflare can refuse it.** ezllm presents itself as Chrome. A refusal shows as
  `403 cloudflare_challenge`; there is no browser fallback.
- **Every request appears in the account's chat list** on claude.ai.

## Add it

1. Sign in to claude.ai in a browser with the test account.
2. Open the developer tools → **Application** → **Cookies** → `https://claude.ai`, and
   copy the value of `sessionKey`.
3. In ezllm, open **Providers** → **Add provider**, pick **Claude.ai (web)**, and paste
   it as `sessionKey=sk-ant-…`. Choose **Save & validate key**.

The check reads the account's organisation, which uses no quota. A pass means the cookie
works.

The website has no model list, so ezllm starts with a few known Claude models. Any other
model ID is passed on too: the answer tells you if the account has it.

## What it can do

- Tools, in the Anthropic shape. Text only: no images, no `response_format`.
- Each request starts a new claude.ai conversation, with the whole history flattened
  into one message. That suits chat; agents lose some structure.

## Not working?

| You see | Do this |
|---|---|
| `401 authentication_error` | The cookie expired or you signed out. Copy a new `sessionKey` and replace the key |
| `403 cloudflare_challenge` | Cloudflare refused the connection. Try again later |
| `403 authentication_error` | The account itself is refused. Check it in a browser |
| `429` | Rate limited. Wait, and send fewer requests at once |
| `502 stream ended before message_stop` | claude.ai stopped mid-answer. Nothing was kept; try again |

---

**How it works:** [claude-web design](design/claude-web-design.md)

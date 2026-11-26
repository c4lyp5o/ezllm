# Use Gemini through its web sign-in

The **Google Gemini (web)** provider sends requests the way gemini.google.com does,
signed in with browser cookies. It is unofficial. For the official API, use
**Google AI Studio**.

Before you start, know the risks:

- **The cookies are your account.** Anyone with them can use the account. ezllm stores
  them encrypted; never put them in `config.yaml`, a chat or a log. If they leak, sign
  the account out of all sessions.
- **Scripted use can get the account locked.** Use a test Google account, never your main
  one.

## Add it

1. Open a private browser window and sign in to gemini.google.com with the test account.
2. Open the developer tools → **Application** (or **Storage**) → **Cookies** →
   `https://gemini.google.com`, and copy the cookies.
3. Close the private window. A browser left open keeps renewing the same session, and
   Google can end a session renewed from two places.
4. In ezllm, open **Providers** → **Add provider**, pick **Google Gemini (web)**, and paste
   the whole cookie header. ezllm keeps `__Secure-1PSID` and `__Secure-1PSIDTS`. Two
   lines, one per cookie, also work. Choose **Save & validate key**.

The check reads the account's status and its models, which uses no quota. The model list
comes from the account itself.

ezllm renews `__Secure-1PSIDTS` every 8 minutes and saves the new value. You do nothing.
If saving fails, the log has a warning and ezllm tries again on a later request.

## What it can do

- Text chat. No tools, no `response_format`, no images: those get
  `400 unsupported_feature`.
- The whole history is flattened into one message. That suits chat; agents lose some
  structure.

## Not working?

| You see | Do this |
|---|---|
| `401 bad_credentials` | The cookies expired or you signed out. Copy new ones and replace the key |
| `403` with upstream `1060` | Google blocked the IP for a while, often a data-centre address. Wait, and do not retry in a loop |
| `429` with upstream `1037` | The account reached its usage limit. Wait for it to reset |
| `503` with upstream `1013` | A short problem at Google. Try again |
| `400 model_not_found` | The account does not have that model. Pick one from its list in **Providers** |
| Upstream `1050` or `1052` | Google changed the website's protocol. Report it as a bug |
| The answer stops halfway | Nothing was kept; try again |

---

**How it works:** [gemini-web design](design/gemini-web-design.md)

# Words in ezllm

The dashboard uses one word for each idea. This page lists them.

The API keeps its own field names, so scripts don't break. The last column shows the
name you'll see there.

| Word | What it means | In the API |
|---|---|---|
| Provider | A service you have a key for, such as OpenAI or OpenRouter. | `account` |
| Provider ID | The start of a provider's model IDs, as in `acme/gpt-x`. Set once, when you add the provider. | `namespace` |
| Provider key | The key the provider gave you. ezllm keeps it encrypted. | `keys` |
| App key | A key ezllm gives you, for your apps to use. | client token |
| Model ID | What an app puts in the `model` field: a provider ID and a model (`acme/gpt-x`), or a combo name. | `model` |
| Combo | A name that stands for a list of models. ezllm tries them in order. | `combo` |
| Model in a combo | One entry in a combo's list. | `hop` |
| Order | How a combo picks from its list. **In order**, the default, tries the first model and moves on when it fails. The others are under **Advanced**. | `strategy` |
| API | The kind of request an app sends: Chat Completions, Anthropic Messages, Responses or Gemini. | `surface`, `wire` |
| Converted | The provider speaks a different API from the app, so ezllm changed the request and the answer. | `translated` |
| Test | One tiny request that checks a model works on an API. | `probe` |
| Force an API | Always send a model's requests as one API. | `pin` |
| Paused | A model or key that failed gets no traffic for a while; then one request tests it again. **Resume** ends it early. | `cooldown` |

## Advanced

Settings most people never need sit behind **Advanced**, closed at first. The dashboard
remembers which ones you open, in this browser.

- **Add provider:** provider ID, key label, pause between tests, notes, session header, custom headers.
- **A provider's models:** each model's result per API, **Force API** and **Test all models**.
- **Combo editor:** order, block size, reset time, compression profile and context size.

How conversion and forcing an API work: [translation runbook](translation-runbook.md).

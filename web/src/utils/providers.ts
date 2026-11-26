// Provider presets and the add-provider call, shared by Providers (the Add
// provider dialog) and Get started.
import { del, post, postKeyTestStream, type Account, type Bridge, type KeyRow, type TestResult, type TestStep } from './api';

export const slugify = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63);

export const PROVIDER_PRESETS: Record<string, {
  label: string; kind: string; baseURL?: string; sessionHeader?: boolean;
  // accountURL builds the base URL from an account ID the operator supplies.
  // Cloudflare is the only one that needs it: the account ID lives INSIDE the
  // endpoint path.
  accountURL?: (id: string) => string;
  // keyHint is the placeholder/one-liner for credentials that are NOT a
  // pasteable API key: a browser cookie pair, a session JSON, an OAuth
  // sessionKey. Shown under the key field when this preset is selected.
  keyHint?: string;
  // multilineKey swaps the masked single-line input for a textarea. Cookie
  // jars arrive as several lines (or one long semicolon header) and simply do
  // not fit one input row — the field must hold them before the backend can
  // extract what it needs.
  multilineKey?: boolean;
  // bridge names an in-process subscription bridge (GET /admin/bridges). The
  // server supplies its key, so the key field becomes a status panel.
  bridge?: Bridge['id'];
  usageConvention?: string;
}> = {
  // Subscription bridges: the official CLI behind a loopback surface. No key to
  // paste — docs/claude-bridge-runbook.md, docs/codex-bridge-runbook.md,
  // docs/agy-bridge-runbook.md.
  'claude-sub': {
    label: 'Claude Pro/Max (bridge)', kind: 'anthropic-compatible', baseURL: 'http://127.0.0.1:8790/v1',
    bridge: 'claude-sub', usageConvention: 'anthropic',
  },
  'chatgpt-sub': {
    label: 'ChatGPT Plus/Pro (bridge)', kind: 'openai-compatible', baseURL: 'http://127.0.0.1:8791/v1',
    bridge: 'chatgpt-sub',
  },
  'antigravity-sub': {
    label: 'Google Antigravity (bridge)', kind: 'openai-compatible', baseURL: 'http://127.0.0.1:8792/v1',
    bridge: 'antigravity-sub',
  },
  // The CLI bridge is its own provider kind, listed under GitHub Copilot.
  'copilot-sub': {
    label: 'GitHub Copilot (bridge)', kind: 'copilot-bridge', baseURL: 'http://127.0.0.1:8793/v1',
    bridge: 'copilot-sub',
  },
  'claude-platform': { label: 'Claude Platform', kind: 'anthropic-compatible', baseURL: 'https://api.anthropic.com' },
  // Web-protocol providers: no API key — credentials are browser sessions.
  // The sentinel base URL is rewritten to the in-process bridge at request
  // time (docs/claude-web-runbook.md, docs/gemini-web-runbook.md).
  'claude-web': {
    label: 'Claude.ai (web)', kind: 'claude-web', baseURL: 'internal://claudeweb',
    keyHint: 'Your claude.ai sessionKey cookie, pasted as sessionKey=sk-ant-… (find it in DevTools → Cookies → claude.ai)',
  },
  'opencode-go': { label: 'OpenCode Go', kind: 'opencode-go', baseURL: 'https://opencode.ai/zen/go/v1', sessionHeader: true },
  'xiaomi-mimo-token-plan': { label: 'Xiaomi MiMo token plan', kind: 'openai-compatible', baseURL: 'https://token-plan-sgp.xiaomimimo.com/v1' },
  'alibaba-model-studio-token-plan': { label: 'Alibaba Cloud Model Studio token plan', kind: 'openai-compatible', baseURL: 'https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1' },
  'commandcode': { label: 'Command Code', kind: 'openai-compatible', baseURL: 'https://api.commandcode.ai/provider/v1' },
  'deepseek': { label: 'DeepSeek', kind: 'openai-compatible', baseURL: 'https://api.deepseek.com' },
  'meta-api': { label: 'Meta Model', kind: 'openai-compatible', baseURL: 'https://api.meta.ai/v1', keyHint: 'Meta Model API key (MODEL_API_KEY)' },
  'groq': { label: 'Groq', kind: 'openai-compatible', baseURL: 'https://api.groq.com/openai/v1' },
  'tokenrouter': { label: 'TokenRouter', kind: 'openai-compatible', baseURL: 'https://api.tokenrouter.com/v1' },
  // Vercel AI Gateway: OpenAI-compatible chat + /models discovery over one key,
  // model ids are `creator/model` (slashes inside the id — splitNamespace
  // splits on the FIRST slash only, so `vercel/anthropic/claude-opus-5` routes
  // exactly like tokenrouter's slash-ids already do).
  'vercel-ai-gateway': { label: 'Vercel AI Gateway', kind: 'openai-compatible', baseURL: 'https://ai-gateway.vercel.sh/v1' },
  'ollama-cloud': { label: 'Ollama Cloud', kind: 'openai-compatible', baseURL: 'https://ollama.com/v1' },
  // Local Ollama speaks the OpenAI wire (and /v1/messages) natively — no
  // adapter. It ignores credentials, but ezllm stores a key per account.
  'ollama-local': {
    label: 'Ollama (local)', kind: 'openai-compatible', baseURL: 'http://localhost:11434/v1',
    keyHint: 'Any value works (for example ollama). A local Ollama ignores the key.',
  },
  'gemini': { label: 'Google AI Studio', kind: 'gemini-openai', baseURL: 'https://generativelanguage.googleapis.com/v1beta/openai' },
  'gemini-web': {
    label: 'Google Gemini (web)', kind: 'gemini-web', baseURL: 'internal://geminiweb',
    multilineKey: true,
    keyHint: 'Paste the whole cookie header (all cookies, separated by semicolons). ezllm keeps __Secure-1PSID and __Secure-1PSIDTS. Two lines also work. Only use a test Google account.',
  },
  'openai': { label: 'OpenAI Platform', kind: 'openai-compatible', baseURL: 'https://api.openai.com/v1' },
  // NVIDIA NIM — standard OpenAI-compatible chat, model ids are bare
  // (e.g. meta/llama-3.3-70b-instruct), so splitNamespace routes them like
  // tokenrouter's slash-ids already do.
  'nvidia-nim': { label: 'NVIDIA NIM', kind: 'openai-compatible', baseURL: 'https://integrate.api.nvidia.com/v1' },
  // Cloudflare Workers AI — the account ID composes the base URL. Workers AI
  // serves the whole OpenAI family under /accounts/{id}/ai/v1: /chat/completions,
  // /responses and an Anthropic-compatible /messages, so nothing here needs a
  // new protocol. Model ids carry their own slashes (@cf/moonshotai/kimi-k2.6).
  'cloudflare-workers-ai': {
    label: 'Cloudflare Workers AI', kind: 'openai-compatible',
    accountURL: (id) => `https://api.cloudflare.com/client/v4/accounts/${encodeURIComponent(id.trim())}/ai/v1`,
  },
  'openrouter': { label: 'OpenRouter', kind: 'openai-compatible', baseURL: 'https://openrouter.ai/api/v1' },
  'openai-compatible': { label: 'OpenAI-compatible', kind: 'openai-compatible' },
  'anthropic-compatible': { label: 'Anthropic-compatible', kind: 'anthropic-compatible' },
};

/** Creates the account, then adds its key through the streamed key test. A
 *  failed test removes the account again, so a bad key leaves nothing behind. */
export async function addProviderWithKey(
  account: Record<string, unknown>,
  key: Record<string, unknown>,
  onStep?: (step: TestStep) => void,
): Promise<{ account: Account; key: KeyRow; test: TestResult }> {
  const created = await post<Account>('/admin/accounts', account);
  try {
    const res = await postKeyTestStream<{ key: KeyRow; test: TestResult }>(
      `/admin/accounts/${created.id}/keys`, key, (step) => onStep?.(step));
    return { account: created, ...res };
  } catch (e) {
    try { await del(`/admin/accounts/${created.id}?force=true`); }
    catch { /* keep the key test's own error: that is the one to show */ }
    throw e;
  }
}

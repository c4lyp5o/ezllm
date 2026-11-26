// Get started in the browser: setup → add a provider → app key → a test
// request sent with that key alone → the snippet. Then the Dashboard nudge.
import { start, json, check, finish } from './mockdash.mjs';

const shots = process.env.SHOTS_DIR;
const t = await start();
let accounts = [];
let tokens = [];
const created = [];
const removed = [];
const chats = [];
const APP_KEY = 'ezllm_live_test_key_0123456789';

const handler = async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  const m = req.method();
  if (p === '/admin/setup' && m === 'GET') { await json(route, { first_boot: true }); return true; }
  if (p === '/admin/setup' && m === 'POST') { await json(route, { ok: true }); return true; }
  if (p === '/admin/accounts' && m === 'GET') { await json(route, accounts); return true; }
  if (p === '/admin/overview') {
    await json(route, { health: { status: 'ok', version: 'test', schema_version: 39, accounts: accounts.length, provider_keys: accounts.length, models: 3, combos: 0, ledger_rows: 0, dropped_rows: 0, uptime_s: 60 },
      usage: [], usage_by_surface: [], accounts, recent_calls: [], block_cursors: [] });
    return true;
  }
  if (p === '/admin/accounts' && m === 'POST') {
    const body = req.postDataJSON();
    created.push(body);
    const a = { id: 10 + created.length, ...body, enabled: true, keys: [], quota: null, models_count: 0 };
    accounts = [...accounts, a];
    await json(route, a, 201);
    return true;
  }
  if (/^\/admin\/accounts\/\d+\/keys$/.test(p)) {
    const key = req.postDataJSON().api_key;
    if (key === 'bad-key') {
      await json(route, { error: { type: 'invalid_request_error', message: 'the provider refused this key (401)', step: 'auth' } }, 422);
      return true;
    }
    await json(route, { key: { id: 1, label: 'primary', hint: 'sk…cdef', enabled: true },
      test: { ok: true, steps: [{ step: 'format', ok: true }, { step: 'catalog', ok: true }, { step: 'auth', ok: true }],
        catalog: [{ id: 'gpt-6-luna' }, { id: 'gpt-6-mini' }, { id: 'gpt-6-nano' }] } }, 201);
    return true;
  }
  if (/^\/admin\/accounts\/\d+$/.test(p) && m === 'DELETE') {
    removed.push(p);
    accounts = accounts.filter((a) => `/admin/accounts/${a.id}` !== p);
    await route.fulfill({ status: 204 });
    return true;
  }
  if (p === '/admin/tokens' && m === 'GET') { await json(route, tokens); return true; }
  if (p === '/admin/tokens' && m === 'POST') {
    const body = req.postDataJSON();
    const tok = { id: 5, name: body.name, hint: 'ezllm…6789', roles: body.roles, enabled: true, cap_window: 'monthly', cap_tokens: 0, cap_usd: 0, allowed_models: '', expires_at: '', created_at: '2026-11-27T00:00:00Z' };
    tokens = [tok];
    await json(route, { token: tok, plaintext: APP_KEY }, 201);
    return true;
  }
  if (p === '/admin/routable-models') {
    await json(route, { data: ['openai/gpt-6-luna', 'openai/gpt-6-mini', 'openai/gpt-6-nano'].map((id) => ({ id })) });
    return true;
  }
  if (p === '/v1/chat/completions') {
    chats.push({ auth: req.headers()['authorization'], cookie: req.headers()['cookie'] ?? '', body: req.postDataJSON() });
    if (req.headers()['authorization'] !== `Bearer ${APP_KEY}`) {
      await json(route, { error: { message: 'invalid token', code: 401 } }, 401);
      return true;
    }
    await json(route, { id: 'c1', choices: [{ index: 0, message: { role: 'assistant', content: 'Hello there, nice to meet!' }, finish_reason: 'stop' }],
      usage: { prompt_tokens: 14, completion_tokens: 7, total_tokens: 21 } });
    return true;
  }
  return false;
};

try {
  // The setup screen says where the code is.
  const gate = await t.page(handler, { unlocked: false });
  await gate.goto(`${t.base}/`);
  await gate.getByText('Set the dashboard password').waitFor();
  check('setup shows how to find the code', await gate.getByText('docker logs ezllm | grep setup').count() === 1);
  if (shots) { await gate.waitForTimeout(800); await gate.screenshot({ path: `${shots}/first-run-0-setup.png` }); }
  await gate.close();

  const page = await t.page(handler);
  // A dashboard session cookie is present, as it is for a signed-in operator: the test request must not send it.
  await page.context().addCookies([{ name: 'ezllm_session', value: 'dashboard-session', url: t.base }]);
  await page.goto(`${t.base}/start`);
  await page.getByRole('heading', { name: 'Add a provider' }).waitFor();
  check('eight provider cards', await page.getByRole('radio').count() === 8);
  check('both kinds of Other', await page.getByText('OpenAI-compatible', { exact: true }).count() === 1 && await page.getByText('Anthropic-compatible', { exact: true }).count() === 1);
  if (shots) { await page.waitForTimeout(800); await page.screenshot({ path: `${shots}/first-run-1-empty.png` }); }

  // A refused key leaves nothing behind.
  await page.getByRole('radio', { name: /OpenAI$/ }).click();
  await page.getByLabel('API key').fill('bad-key');
  await page.getByRole('button', { name: 'Connect' }).click();
  await page.getByText('the provider refused this key (401)').waitFor();
  check('a refused key removes the account again', removed.length === 1 && accounts.length === 0, { removed, accounts: accounts.length });

  await page.getByLabel('API key').fill('sk-real-key');
  check('a new key clears the old error', await page.getByText('the provider refused this key (401)').count() === 0);
  if (shots) await page.screenshot({ path: `${shots}/first-run-2-provider.png` });
  await page.getByRole('button', { name: 'Connect' }).click();
  await page.getByRole('heading', { name: 'Create a key for your app' }).waitFor();
  await page.getByText('OpenAI · 3 models').waitFor();
  const sent = created.at(-1);
  check('the provider is created from the preset', sent.name === 'OpenAI' && sent.namespace === 'openai' && sent.base_url === 'https://api.openai.com/v1' && sent.kind === 'openai-compatible', sent);

  await page.getByRole('button', { name: 'Create key' }).click();
  await page.getByRole('heading', { name: 'Send a test request' }).waitFor();
  check('the new key is shown once, with a warning', await page.locator('code', { hasText: APP_KEY }).count() === 1 && await page.getByText('It is shown only once.', { exact: false }).count() === 1);
  await page.waitForFunction(() => document.querySelector('#start-model')?.value === 'openai/gpt-6-luna');
  check('the test model comes from the new provider', true);

  await page.getByRole('button', { name: 'Send a test request' }).click();
  await page.getByText('Hello there, nice to meet!').waitFor();
  const call = chats.at(-1);
  check('the test request uses the app key alone', call.auth === `Bearer ${APP_KEY}` && call.cookie === '', call);
  check('the result shows tokens', await page.getByText(/21 tokens/).count() === 1);
  check('the snippet carries the address, key and model', (await page.locator('pre').innerText()).includes(`OPENAI_API_KEY=${APP_KEY}`) && (await page.locator('pre').innerText()).includes('openai/gpt-6-luna'));
  await page.getByRole('radio', { name: 'curl' }).click().catch(async () => { await page.getByRole('button', { name: 'curl' }).click(); });
  check('curl snippet', (await page.locator('pre').innerText()).includes('/v1/chat/completions'));
  await page.getByRole('heading', { name: "You're set." }).waitFor();
  if (shots) { await page.waitForTimeout(800); await page.screenshot({ path: `${shots}/first-run-3-done.png`, fullPage: true }); }

  // The Dashboard nudges while unfinished, and not once everything is done.
  tokens = [];
  await page.goto(`${t.base}/`);
  await page.getByText('Finish setting up').waitFor();
  check('dashboard shows progress', await page.getByText('1 of 3 done').first().isVisible());
  if (shots) { await page.waitForTimeout(800); await page.screenshot({ path: `${shots}/first-run-4-banner.png`, clip: { x: 0, y: 0, width: 1440, height: 260 } }); }
  tokens = [{ id: 5, name: 'my-app', roles: ['infer'], enabled: true, last_used_at: '2026-11-27T00:00:00Z' }];
  await page.reload();
  await page.waitForTimeout(1500);
  check('no nudge once all three are done', await page.getByText('Finish setting up').count() === 0);
} finally {
  await t.close();
}
finish();

// Screenshot helper for the Combos page (desktop + phone, list + editor).
import { start, json } from './mockdash.mjs';
const out = process.argv[2] ?? '/tmp';
const t = await start();
const acct = (id, name, host) => ({ id, name, namespace: name.toLowerCase(), kind: 'openai-compatible', base_url: `https://${host}/v1`, enabled: true, keys: [{ id, label: 'k', hint: 'x', enabled: true, last_test_ok: true }], quota: null, models_count: 3, rate_limit: 3, rate_window: '60s', protocol_support: { chat: 2, anthropic: 0, responses: 0, untested: 0 } });
const accounts = [acct(1, 'Groq-1', 'api.groq.com'), { ...acct(2, 'Groq-2', 'api.groq.com'), enabled: false }, acct(3, 'OpenRouter', 'openrouter.ai')];
const hop = (a, m, o = {}) => ({ account_id: a, model_id: m, weight: 1, enabled: true, ...o });
const combos = [
  { id: 1, name: 'daily', strategy: 'failover', sticky_idle_s: null, enabled: true, block_size: 1, context_size: 200000, compression_profile_id: 1, hops: [hop(1, 'llama-3.3-70b'), hop(2, 'llama-3.3-70b'), hop(3, 'deepseek/deepseek-r1', { enabled: false })] },
  { id: 2, name: 'cache-warm', strategy: 'block_round_robin', sticky_idle_s: null, enabled: true, block_size: 5, context_size: 200000, hops: [hop(1, 'a', { hold_until_failure: true }), hop(2, 'b'), hop(3, 'c')] },
  { id: 3, name: 'free', strategy: 'least_used', sticky_idle_s: null, enabled: false, block_size: 1, context_size: 128000, auto_free: true, hops: [hop(1, 'x:free'), hop(3, 'y:free')] },
];
const page = await t.page(async (route, url) => {
  const p = url.pathname;
  if (p === '/admin/combos') { await json(route, combos); return true; }
  if (p === '/admin/accounts') { await json(route, accounts); return true; }
  if (p === '/admin/compression-profiles') { await json(route, [{ id: 1, name: 'lean' }]); return true; }
  if (p === '/admin/usage') { await json(route, { groups: [{ k: 'daily', calls: 1204, errors: 36, tin: 1, tout: 1 }, { k: 'cache-warm', calls: 80, errors: 0 }], totals: {} }); return true; }
  if (p === '/admin/overview') { await json(route, { block_cursors: [{ combo: 'cache-warm', hop: 'groq-1/a', remaining: 2, block_size: 5, reason: 'cold start' }] }); return true; }
  if (/accounts\/\d+\/models/.test(p)) { await json(route, [{ id: 'a' }, { id: 'b' }]); return true; }
  return false;
});
await page.goto(`${t.base}/combos`);
await page.getByText('daily').first().waitFor();
await page.waitForTimeout(600);
await page.screenshot({ path: `${out}/combos-list.png` });
await page.screenshot({ path: `${out}/combos-list-narrow.png`, clip: { x: 220, y: 0, width: 1220, height: 700 } });
await page.getByRole('button', { name: 'Edit' }).nth(1).click();
await page.getByRole('dialog').waitFor();
await page.waitForTimeout(600);
await page.screenshot({ path: `${out}/combos-edit.png` });
await page.getByRole('dialog').locator('div.overflow-y-auto').first().evaluate((el) => { el.scrollTop = el.scrollHeight; });
await page.waitForTimeout(300);
await page.screenshot({ path: `${out}/combos-edit-bottom.png` });
await page.keyboard.press('Escape');
await page.setViewportSize({ width: 420, height: 900 });
await page.waitForTimeout(300);
await page.screenshot({ path: `${out}/combos-phone.png`, fullPage: true });
await t.close();

// Screenshot helper for the Connect page (list, create modal, show-once; desktop and phone).
import { start, json } from './mockdash.mjs';
const out = process.argv[2] ?? '/tmp';
const t = await start();
const day = 86_400_000;
const iso = (ms) => new Date(Date.now() + ms).toISOString();
const tok = (id, name, o = {}) => ({ id, name, hint: `ez-…${id}${id}${id}${id}`, roles: ['infer'], enabled: true, cap_window: 'monthly', cap_tokens: 0, allowed_models: '', expires_at: '', last_used_at: iso(-2 * 3600e3), created_at: iso(-30 * day), ...o });
const tokens = [tok(1, 'hermes', { cap_tokens: 10_000_000, allowed_models: 'deepseek/*', expires_at: iso(200 * day) }), tok(2, 'old', { expires_at: iso(-2 * day) }), tok(3, 'soon', { expires_at: iso(3 * day) }), tok(4, 'spent', { cap_tokens: 1000 }), tok(6, 'boss', { roles: ['admin'] })];
const page = await t.page(async (route, url) => {
  const p = url.pathname;
  if (p === '/admin/tokens' && route.request().method() === 'GET') { await json(route, tokens); return true; }
  if (p === '/admin/tokens') { await json(route, { token: tok(9, 'laptop'), plaintext: 'ez-secret-123' }, 201); return true; }
  if (p === '/admin/usage') { await json(route, { groups: [{ k: 'hermes', calls: 1204, tin: 2000000, tout: 500000 }, { k: 'spent', calls: 3, tin: 1500, tout: 600 }], totals: {} }); return true; }
  if (p === '/admin/routable-models') { await json(route, { data: [{ id: 'deepseek/chat' }, { id: 'deepseek/coder' }, { id: 'openai/gpt-5' }, { id: 'daily' }] }); return true; }
  return false;
});
await page.goto(`${t.base}/connect`);
await page.getByRole('switch', { name: 'Disable key hermes' }).waitFor();
await page.waitForTimeout(800);
await page.screenshot({ path: `${out}/connect-list.png`, fullPage: true });
await page.getByRole('button', { name: /Create key/ }).first().click();
await page.getByRole('dialog').waitFor();
await page.getByLabel('Name', { exact: true }).fill('laptop');
await page.locator('#tk-models').fill('deepseek/*');
await page.getByRole('button', { name: '90 days', exact: true }).click();
await page.getByRole('button', { name: '1,000,000', exact: true }).click();
await page.waitForTimeout(400);
await page.screenshot({ path: `${out}/connect-create.png` });
await page.getByRole('button', { name: 'Create key', exact: true }).click();
await page.getByRole('dialog', { name: 'New API key' }).waitFor();
await page.waitForTimeout(500);
await page.screenshot({ path: `${out}/connect-fresh.png` });
await page.getByRole('button', { name: 'I saved it' }).click();
await page.setViewportSize({ width: 420, height: 900 });
await page.waitForTimeout(400);
await page.screenshot({ path: `${out}/connect-phone.png`, fullPage: true });
await t.close();

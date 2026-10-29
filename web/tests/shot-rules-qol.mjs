// Screenshot helper for the Rules page (list + editor, desktop and phone).
import { start, json } from './mockdash.mjs';
const out = process.argv[2] ?? '/tmp';
const t = await start();
const acct = (id, name, ns, o = {}) => ({ id, name, namespace: ns, kind: 'openai-compatible', base_url: `https://${ns}.test/v1`, enabled: true, keys: [], quota: null, models_count: 3, ...o });
const accounts = [acct(1, 'Groq', 'groq'), acct(2, 'OpenRouter', 'openrouter', { enabled: false })];
const rule = (id, a, m, o = {}) => ({ id, account_id: a, model_id: m, cap_tokens: 0, cap_window: 'monthly', win_start: '', win_end: '', win_days: '', win_tz: 'instance', enabled: true, note: '', ...o });
const many = ['m1', 'm2', 'm3', 'm4', 'm5', 'm6', 'm7'];
const rules = [
  ...many.map((m, i) => rule(10 + i, 1, m, { cap_tokens: 5000000, win_start: '09:00', win_end: '18:00', win_days: '1,2,3,4,5', win_tz: 'Asia/Kuala_Lumpur', note: 'day models' })),
  rule(30, 1, 'gone-model', { cap_tokens: 100000, cap_window: 'daily' }),
  rule(31, 2, 'x', { win_start: '22:00', win_end: '06:00', win_days: '0,6' }),
  rule(32, 1, 'a', { enabled: false }),
];
const page = await t.page(async (route, url) => {
  const p = url.pathname;
  if (p === '/admin/model-rules') { await json(route, rules); return true; }
  if (p === '/admin/accounts') { await json(route, accounts); return true; }
  if (/accounts\/\d+\/models/.test(p)) { await json(route, [...many, 'a', 'x'].map((id) => ({ id }))); return true; }
  return false;
});
await page.goto(`${t.base}/rules`);
await page.getByRole('button', { name: 'Edit', exact: true }).first().waitFor();
await page.waitForTimeout(700);
await page.screenshot({ path: `${out}/rules-list.png` });
await page.getByRole('button', { name: 'Edit', exact: true }).first().click();
await page.getByRole('dialog').waitFor();
await page.waitForTimeout(600);
await page.screenshot({ path: `${out}/rules-edit.png` });
await page.getByRole('dialog').locator('div.overflow-y-auto').first().evaluate((el) => { el.scrollTop = el.scrollHeight; });
await page.waitForTimeout(300);
await page.screenshot({ path: `${out}/rules-edit-bottom.png` });
await page.keyboard.press('Escape');
await page.getByRole('dialog').getByRole('button', { name: 'Discard' }).click().catch(() => {});
await page.setViewportSize({ width: 420, height: 900 });
await page.waitForTimeout(300);
await page.screenshot({ path: `${out}/rules-phone.png`, fullPage: true });
await t.close();

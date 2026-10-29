// Screenshot helper for the Neighbourhood page (desktop and phone).
import { start, json, account } from './mockdash.mjs';
const out = process.argv[2] ?? '/tmp';
const t = await start();
const key = (id, label, hint, o = {}) => ({ id, label, hint, enabled: true, last_test_ok: true, ...o });
const accounts = [
  { ...account, id: 1, name: 'Alpha Gateway', namespace: 'alpha', keys: [key(1, 'busy', 'a')] },
  { ...account, id: 2, name: 'Beta Works', namespace: 'beta', keys: [key(2, 'flaky', 'b', { last_test_ok: false })] },
  { ...account, id: 3, name: 'Gamma Cloud', namespace: 'gamma', keys: [key(3, 'idle', 'c')] },
  { ...account, id: 4, name: 'Delta Offline', namespace: 'delta', enabled: false, keys: [key(4, 'old', 'd')] },
];
const row = (k, calls, tin, errors = 0) => ({ k, calls, tin, tout: 0, errors, cread: 0, cwrite: 0, reasoning: 0, saved: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/accounts') { await json(route, accounts); return true; }
  if (url.pathname === '/admin/usage') { await json(route, { groups: [row('a', 40, 120000), row('b', 8, 5000, 4)], totals: {} }); return true; }
  return false;
});
await page.goto(`${t.base}/neighbourhood?select=2`);
await page.locator('tbody tr[data-row]').first().waitFor();
await page.waitForTimeout(1200);
await page.mouse.move(5, 5);
await page.screenshot({ path: `${out}/nb-desktop.png`, fullPage: true });
await page.setViewportSize({ width: 420, height: 900 });
await page.waitForTimeout(600);
await page.screenshot({ path: `${out}/nb-phone.png`, fullPage: true });
await t.close();

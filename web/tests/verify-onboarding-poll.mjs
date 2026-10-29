// Verify bridge-friendly first request and visible, non-overlapping polling.
import { start, json, check, finish } from './mockdash.mjs';
const t = await start();
let notices = 0, backups = 0, hold = null;
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/routable-models') { await json(route, { data: [{ id: "unsupported/model", protocol: { chat: false } }, { id: "demo/model's" }] }); return true; }
  if (url.pathname === '/admin/settings') { await json(route, { revision: 0, fields: {} }); return true; }
  if (url.pathname === '/admin/notifications') { notices++; await json(route, { items: [], unread: 0 }); return true; }
  if (url.pathname === '/admin/backups') { backups++; if (hold) await hold; await json(route, { ready: true, running: false, backups: [] }); return true; }
  return false;
});
await page.addInitScript(() => {
  window.__testHidden = false;
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.__testHidden });
  window.__pollCallbacks = {};
  const original = window.setInterval;
  window.setInterval = (callback, ms, ...args) => {
    if (ms === 5000 || ms === 60000) { window.__pollCallbacks[ms] = callback; return 999; }
    return original(callback, ms, ...args);
  };
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => { window.__copied = text; } } });
});
await page.goto(`${t.base}/connect`);
await page.getByText(/Create an inference-only key above/).waitFor();
await page.getByRole('button', { name: 'Copy request' }).click();
const copied = await page.evaluate(() => window.__copied);
check('example uses discovered model and a key placeholder', copied.includes('demo/model') && copied.includes('YOUR_EZLLM_KEY') && copied.includes('/v1/chat/completions'), copied);
check('example escapes single quotes in model IDs', copied.includes("'\\''"));
check('example does not contain a live credential', !copied.includes('tok-admin'));

await page.goto(`${t.base}/settings`);
await page.getByRole('button', { name: 'Backup & Recovery', exact: true }).click();
await page.getByText('Backup storage configured').waitFor();
const beforeHidden = { notices, backups };
await page.evaluate(() => { window.__testHidden = true; window.__pollCallbacks[5000](); window.__pollCallbacks[60000](); });
await page.waitForTimeout(150);
check('hidden tab skips backup and notification polls', notices === beforeHidden.notices && backups === beforeHidden.backups, { notices, backups });
let release;
hold = new Promise(resolve => { release = resolve; });
await page.evaluate(() => { window.__testHidden = false; document.dispatchEvent(new Event('visibilitychange')); window.__pollCallbacks[5000](); window.__pollCallbacks[5000](); });
await page.waitForTimeout(200);
check('visible tab refreshes once without overlap', backups === beforeHidden.backups + 1 && notices === beforeHidden.notices + 1, { notices, backups });
release(); hold = null;
await t.close();
finish();

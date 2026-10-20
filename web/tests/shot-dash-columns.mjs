// Recent-calls table layout check: desktop screenshot + the mobile overflow
// walk (scrollWidth vs clientWidth on every route; pan must stay at 0).
// Run against a scratch verify instance: node web/shot-dash-columns.mjs
import { chromium } from 'playwright';

const BASE = process.env.EZLLM_SHOT_BASE || 'http://127.0.0.1:20199';
// The unlock gate wants the DASHBOARD PASSWORD (bcrypt), not the admin bearer.
// There is no factory password: a scratch copy carries prod's hash, so reset
// it (DELETE FROM dashboard_auth) and start the verify instance with
// EZLLM_DASHBOARD_PASSWORD set to the same value as EZLLM_SHOT_PW.
const PW = process.env.EZLLM_SHOT_PW;
if (!PW) throw new Error('set EZLLM_SHOT_PW (and EZLLM_DASHBOARD_PASSWORD on the instance)');
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
const ROUTES = ['/', '/stats', '/economics', '/requests', '/providers', '/combos',
  '/rules', '/compression', '/connect', '/chat', '/settings'];

const b = await chromium.launch();

// ── desktop: the consolidated recent-calls table ───────────────────────────
const ctx = await b.newContext({ viewport: { width: 1400, height: 1000 } });
const pg = await ctx.newPage();
await pg.goto(BASE + '/');
await pg.waitForTimeout(600);
const inp = pg.locator('input[type=password], input[placeholder*=token i]');
if (await inp.count()) { await inp.first().fill(PW); await pg.keyboard.press('Enter'); await pg.waitForTimeout(900); }
await pg.goto(BASE + '/');
await pg.waitForTimeout(1500);

// Table identity first: the catch-all route renders Dashboard for any path, so
// confirm the headings before trusting a capture.
const heads = await pg.locator('th').allInnerTexts();
console.log('HEADERS=' + JSON.stringify(heads.map((h) => h.trim()).filter(Boolean)));

const card = pg.locator('div.rounded-xl', { has: pg.locator('table') }).last();
if (await card.count()) {
  await card.screenshot({ path: shots + '/dash-columns.png' });
  console.log('SHOT_CARD_OK');
} else {
  await pg.locator('table').last().screenshot({ path: shots + '/dash-columns.png' });
  console.log('SHOT_TABLE_OK');
}

// The first data row, as text, so the stacked cells can be checked without eyes.
const firstRow = await pg.locator('tbody tr').first().innerText().catch(() => '');
console.log('FIRST_ROW=' + JSON.stringify(firstRow.replace(/\n/g, ' | ')));
await ctx.close();

// ── mobile: no horizontal overflow on any route ────────────────────────────
const mctx = await b.newContext({
  viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true,
  deviceScaleFactor: 2,
});
const mp = await mctx.newPage();
await mp.goto(BASE + '/');
await mp.waitForTimeout(600);
const minp = mp.locator('input[type=password], input[placeholder*=token i]');
if (await minp.count()) { await minp.first().fill(PW); await mp.keyboard.press('Enter'); await mp.waitForTimeout(900); }

let bad = 0;
for (const r of ROUTES) {
  await mp.goto(BASE + r);
  await mp.waitForTimeout(1200);
  const m = await mp.evaluate(() => {
    const d = document.documentElement;
    window.scrollTo(9999, 0);
    return { sw: d.scrollWidth, cw: d.clientWidth, x: window.scrollX };
  });
  const ok = m.sw <= m.cw && m.x === 0;
  if (!ok) bad++;
  console.log(`MOBILE ${r.padEnd(14)} scrollWidth=${m.sw} clientWidth=${m.cw} scrollX=${m.x} ${ok ? 'ok' : 'OVERFLOW'}`);
}
console.log('MOBILE_OVERFLOW_ROUTES=' + bad);
await mp.goto(BASE + '/');
await mp.waitForTimeout(1200);
await mp.screenshot({ path: shots + '/dash-columns-mobile.png', fullPage: true });
console.log('SHOT_MOBILE_OK');

await mctx.close();
await b.close();
console.log('DONE');

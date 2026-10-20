// Test-all sweep UI: the models drawer with the new status column and the
// Test-all control, plus the mobile overflow check with the drawer OPEN.
import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';

const BASE = process.env.EZLLM_SHOT_BASE || 'http://127.0.0.1:20199';
// No factory password: start the instance with EZLLM_DASHBOARD_PASSWORD equal
// to EZLLM_SHOT_PW (see shot-dash-columns.mjs).
const PW = process.env.EZLLM_SHOT_PW;
if (!PW) throw new Error('set EZLLM_SHOT_PW (and EZLLM_DASHBOARD_PASSWORD on the instance)');
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
const TARGET = process.env.EZLLM_SHOT_ACCOUNT || 'space-stationine';

const b = await chromium.launch();
const ctx = await b.newContext({ viewport: { width: 1400, height: 1000 } });
const pg = await ctx.newPage();
await pg.goto(BASE + '/providers');
await pg.waitForTimeout(600);
const inp = pg.locator('input[type=password], input[placeholder*=token i]');
if (await inp.count()) { await inp.first().fill(PW); await pg.keyboard.press('Enter'); await pg.waitForTimeout(1000); }
await pg.goto(BASE + '/providers');
await pg.waitForTimeout(1800);

// Confirm page identity first (the catch-all route would happily render the
// Dashboard for a wrong URL).
const heading = await pg.locator('h1').first().innerText().catch(() => '');
console.log('PAGE_HEADING=' + JSON.stringify(heading));

const card = pg.locator('button[aria-expanded]').filter({ hasText: TARGET }).first();
console.log('CARD_MATCHES=' + await card.count());
if (await card.count()) {
  await card.click();
  await pg.waitForTimeout(1200);
}
const open = pg.locator('button[aria-expanded="true"]');
if (await open.count()) {
  const drawer = pg.locator('div', { has: pg.locator('table') }).last();
  await drawer.screenshot({ path: shots + '/providers-sweep.png' }).catch(async () => {
    await pg.screenshot({ path: shots + '/providers-sweep.png' });
  });
  console.log('SHOT_OK');
  const txt = (await pg.locator('button[aria-expanded="true"]').first().locator('xpath=following-sibling::*[1]').innerText().catch(() => ''))
    .replace(/\n/g, ' | ');
  console.log('DRAWER=' + JSON.stringify(txt.slice(0, 700)));
}
const btns = await pg.locator('button').allInnerTexts();
console.log('HAS_TEST_ALL=' + JSON.stringify(btns.some((t) => /Test all models/.test(t))));
console.log('HAS_STATUS_COL=' + JSON.stringify(await pg.locator('th', { hasText: /^status$/i }).count() > 0));
await ctx.close();

// Mobile with the drawer open: the drawer is inside an account card, so a
// wide status line is the most likely new overflow.
const mctx = await b.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
const mp = await mctx.newPage();
await mp.goto(BASE + '/providers');
await mp.waitForTimeout(600);
const minp = mp.locator('input[type=password], input[placeholder*=token i]');
if (await minp.count()) { await minp.first().fill(PW); await mp.keyboard.press('Enter'); await mp.waitForTimeout(1000); }
await mp.goto(BASE + '/providers');
await mp.waitForTimeout(1800);
const mcard = mp.locator('button[aria-expanded]').filter({ hasText: TARGET }).first();
if (await mcard.count()) { await mcard.click(); await mp.waitForTimeout(1200); }
const m = await mp.evaluate(() => {
  const d = document.documentElement;
  window.scrollTo(9999, 0);
  return { sw: d.scrollWidth, cw: d.clientWidth, x: window.scrollX };
});
console.log(`MOBILE_DRAWER_OPEN scrollWidth=${m.sw} clientWidth=${m.cw} scrollX=${m.x} ${m.sw <= m.cw && m.x === 0 ? 'ok' : 'OVERFLOW'}`);
await mp.screenshot({ path: shots + '/providers-sweep-mobile.png', fullPage: true });
await mctx.close();
await b.close();
console.log('DONE');

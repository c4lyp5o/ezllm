// Dashboard check for the hop stream-health card (docs/hop-health-design.md).
// Asserts the card renders the server's numbers, the attention item fires for a
// hop over 1%, and the page still has no mobile overflow.
//
// Run from web/: RIG_PASS=... node tests/shot-hop-health.mjs

import { chromium } from 'playwright';
import fs from 'node:fs';

const BASE = process.env.EZLLM_BASE ?? 'http://127.0.0.1:20199';
const PASS = process.env.RIG_PASS;
const SHOTS = process.env.SHOTS_DIR ?? `${process.env.HOME}/.hermes/cache/scratch/ezllm-verify/shots`;
if (!PASS) { console.error('RIG_PASS required'); process.exit(2); }
fs.mkdirSync(SHOTS, { recursive: true });

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
const errors = [];
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));

await page.goto(BASE, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(1200);
if (await page.locator('#dashboard-password').count()) {
  await page.fill('#dashboard-password', PASS);
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForTimeout(2000);
}
await page.waitForSelector('text=Cut answers', { timeout: 15000 });

const card = page.locator('section', { hasText: 'Cut answers' }).last();
await card.scrollIntoViewIfNeeded();
const cardText = (await card.innerText()).replace(/\s+/g, ' ');
console.log('CARD:', cardText);

const attention = page.locator('section[aria-label="Needs attention"]');
const attnText = (await attention.count()) ? (await attention.innerText()).replace(/\s+/g, ' ') : '(none)';
console.log('ATTENTION:', attnText);

await page.screenshot({ path: `${SHOTS}/hop-health-dashboard.png`, fullPage: true });
card && await card.screenshot({ path: `${SHOTS}/hop-health-card.png` });

// mobile: the new card must not widen the page
await page.setViewportSize({ width: 390, height: 844 });
await page.goto(BASE, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(1800);
const w = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth }));
console.log(`MOBILE: ${w.scroll} vs ${w.client}`);
await page.screenshot({ path: `${SHOTS}/hop-health-mobile.png`, fullPage: false });

const checks = {
  cardHasRate: /3\.2%/.test(cardText),
  cardExcludesHangups: /2 cut of 62/.test(cardText),
  attentionNamesHop: /demo-hop/.test(attnText) && /3\.2%/.test(attnText),
  noConsoleErrors: errors.length === 0,
  noMobileOverflow: w.scroll <= w.client + 1,
};
console.log('CHECKS:', JSON.stringify(checks));
if (errors.length) console.log('ERRORS:', errors.slice(0, 5));
await browser.close();
process.exit(Object.values(checks).every(Boolean) ? 0 : 1);

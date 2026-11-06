// Runtime smoke of the built SPA against a loopback verify instance.
// Proves the dep upgrade (React 19, Vite 8/rolldown, recharts 3, TS 7) renders
// every route without a console error or a React crash — a green tsc/vite
// build says nothing about runtime.
//
// Run from web/: node tests/shot-dep-upgrade-smoke.mjs
// Env: EZLLM_BASE (default http://127.0.0.1:20199),
//      RIG_PASS  (dashboard password of the rig; never a prod secret)

import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';

const BASE = process.env.EZLLM_BASE ?? 'http://127.0.0.1:20199';
const PASS = process.env.RIG_PASS;
const SHOTS = process.env.SHOTS_DIR ?? `${process.env.HOME}/.hermes/cache/scratch/ezllm-verify/shots`;
const ROUTES = ['/', '/stats', '/economics', '/requests', '/providers', '/services',
  '/chat', '/combos', '/rules', '/compression', '/connect', '/neighbourhood', '/settings'];

if (!PASS) { console.error('RIG_PASS is required (read it from the rig launch, never hardcode)'); process.exit(2); }
fs.mkdirSync(SHOTS, { recursive: true });

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [];
page.on('console', (m) => { if (m.type() === 'error') errors.push(`console: ${m.text()}`); });
page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));

await page.goto(BASE, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(1200);
if (await page.locator('#dashboard-password').count()) {
  await page.fill('#dashboard-password', PASS);
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForTimeout(1500);
}
const unlocked = !(await page.locator('#dashboard-password').count());
console.log(`unlocked=${unlocked}`);

const rows = [];
for (const route of ROUTES) {
  errors.length = 0;
  const resp = await page.goto(BASE + route, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1600);
  const h1 = await page.locator('h1').first().innerText().catch(() => '(none)');
  const svg = await page.locator('svg.recharts-surface').count();
  const crashed = /something went wrong|unexpected error|error boundary/i.test(await page.locator('body').innerText());
  const bodyLen = (await page.locator('body').innerText()).length;
  rows.push({ route, status: resp?.status(), h1: h1.replace(/\s+/g, ' ').slice(0, 40), charts: svg, crashed, bodyLen, errors: [...errors] });
  if (route === '/' || route === '/providers') await page.screenshot({ path: path.join(SHOTS, `deps-${route.replace(/\W/g, '') || 'root'}.png`), fullPage: false });
}

let bad = 0;
for (const r of rows) {
  const flag = r.crashed || r.errors.length || r.status !== 200 || r.bodyLen < 40 ? 'FAIL' : 'ok';
  if (flag === 'FAIL') bad++;
  console.log(`${flag.padEnd(4)} ${r.route.padEnd(16)} status=${r.status} h1="${r.h1}" charts=${r.charts} body=${r.bodyLen}` +
    (r.errors.length ? `\n     ${r.errors.slice(0, 3).join('\n     ')}` : ''));
}

// mobile overflow walk (390x844) — a layout claim needs this, not a desktop shot
await page.setViewportSize({ width: 390, height: 844 });
let overflow = 0;
for (const route of ROUTES) {
  await page.goto(BASE + route, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(900);
  const w = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth }));
  if (w.scroll > w.client + 1) { overflow++; console.log(`MOBILE OVERFLOW ${route}: ${w.scroll} > ${w.client}`); }
}
console.log(`routes_failed=${bad} mobile_overflow=${overflow} shots=${SHOTS}`);
await browser.close();
process.exit(bad || overflow ? 1 : 0);

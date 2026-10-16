// shot-nullms.mjs — verify the dashboard renders en-dash "–" (not "nullms") for unmeasured ttft, and screenshot it.
// Auth: injects the admin token into localStorage before page scripts run (SPA reads TOKEN_KEY 'ezllm.admin.token').
import { chromium } from 'playwright';
import { readFileSync, mkdirSync } from 'node:fs';

const TOKEN = readFileSync('/home/calypso/.hermes/secrets/ezllm-admin.token', 'utf8').trim();
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
mkdirSync(shots, { recursive: true });

const b = await chromium.launch();
const ctx = await b.newContext({ viewport: { width: 1440, height: 1100 } });
await ctx.addInitScript((t) => {
  try { localStorage.setItem('ezllm.admin.token', t); } catch (e) { /* about:blank has no storage */ }
}, TOKEN);

const pg = await ctx.newPage();
await pg.goto('http://127.0.0.1:20129/');
await pg.waitForTimeout(2600);

const txt = await pg.evaluate(() => document.body.innerText);
const nullms = (txt.match(/null\s*ms/gi) || []).length;
const undef = (txt.match(/undefined\s*ms/gi) || []).length;
const endash = (txt.match(/\u2013/g) || []).length;
console.log(`NULLMS=${nullms} UNDEFMS=${undef} EN_DASHES=${endash}`);
console.log('snippet=' + JSON.stringify(txt.slice(0, 140)));

const rowTexts = await pg.evaluate(() =>
  Array.from(document.querySelectorAll('tr')).map((r) => r.innerText.replace(/\s+/g, ' ').slice(0, 140)));
const badRows = rowTexts.filter((t) => /null\s*ms|undefined\s*ms/i.test(t));
console.log(`ROWS=${rowTexts.length} BAD_ROWS=${badRows.length}`);
if (rowTexts.length) console.log('sample=' + rowTexts.slice(0, 4).join(' || '));

await pg.screenshot({ path: `${shots}/dash-nullms-full.png`, fullPage: true });
console.log('SHOT_FULL=' + shots + '/dash-nullms-full.png');
try {
  const card = pg.locator('div').filter({ hasText: /recent calls/i }).last();
  await card.screenshot({ path: `${shots}/dash-nullms-recent.png`, timeout: 5000 });
  console.log('SHOT_RECENT=' + shots + '/dash-nullms-recent.png');
} catch (e) {
  console.log('NO_RECENT_CARD: ' + String(e).slice(0, 100));
}
await b.close();
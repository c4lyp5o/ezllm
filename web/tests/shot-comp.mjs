import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';
// the admin token lives in the secrets dir; never hardcode it here.
const TOKEN = readFileSync('/home/calypso/.hermes/secrets/ezllm-admin.token', 'utf8').trim();
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
const b = await chromium.launch();
const pg = await (await b.newContext({ viewport: { width: 1400, height: 1000 } })).newPage();
await pg.goto('http://127.0.0.1:20199/');
await pg.waitForTimeout(600);
const inp = pg.locator('input[type=password], input[placeholder*=token i]');
if (await inp.count()) { await inp.first().fill(TOKEN); await pg.keyboard.press('Enter'); await pg.waitForTimeout(900); }
await pg.goto('http://127.0.0.1:20199/compression');
await pg.waitForTimeout(900);
await pg.screenshot({ path: shots + '/compression.png', fullPage: true });
await pg.goto('http://127.0.0.1:20199/stats');
await pg.waitForTimeout(700);
const tab = pg.locator('button:has-text("Compression")');
if (await tab.count()) { await tab.first().click(); await pg.waitForTimeout(1000); }
await pg.screenshot({ path: shots + '/stats-compression.png', fullPage: true });
await pg.goto('http://127.0.0.1:20199/stats');
await pg.waitForTimeout(1200);
await pg.screenshot({ path: shots + '/feed.png', fullPage: true });
await pg.close(); await b.close();
console.log('SHOTS_OK');

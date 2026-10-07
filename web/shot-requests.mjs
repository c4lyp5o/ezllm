import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';
const TOKEN = readFileSync('/home/calypso/.hermes/secrets/ezllm-admin.token', 'utf8').trim();
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
const b = await chromium.launch();
const pg = await (await b.newContext({ viewport: { width: 1400, height: 1000 } })).newPage();
await pg.goto('http://127.0.0.1:20199/');
await pg.waitForTimeout(600);
const inp = pg.locator('input[type=password], input[placeholder*=token i]');
if (await inp.count()) { await inp.first().fill(TOKEN); await pg.keyboard.press('Enter'); await pg.waitForTimeout(900); }

// unfiltered first
await pg.goto('http://127.0.0.1:20199/requests');
await pg.waitForTimeout(1100);
await pg.screenshot({ path: shots + '/requests.png', fullPage: true });

// then apply a filter: tokens in >= 1 (drops the zero-token error rows)
const minTin = pg.locator('input[placeholder="e.g. 50000"]');
if (await minTin.count()) {
  await minTin.first().fill('1');
  await pg.locator('button:has-text("Apply")').click();
  await pg.waitForTimeout(900);
  await pg.screenshot({ path: shots + '/requests-filtered.png', fullPage: true });
}
await pg.close(); await b.close();
console.log('REQUESTS_SHOTS_OK');

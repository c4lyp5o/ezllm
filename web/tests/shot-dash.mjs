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
await pg.goto('http://127.0.0.1:20199/');
await pg.waitForTimeout(1500);
// Recent calls card = bordered div wrapping the table (not a <section>)
const card = pg.locator('div.rounded-xl', { has: pg.locator('table') }).last();
if (await card.count()) {
  await card.screenshot({ path: shots + '/dash-recent.png' });
  console.log('CARD_OK');
} else {
  await pg.locator('table').last().screenshot({ path: shots + '/dash-recent.png' });
  console.log('TABLE_OK');
}
await pg.close(); await b.close();
